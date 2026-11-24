#!/usr/bin/env python3
"""Audit Claude Code request records for one Unreal main session, read-only."""

import argparse
from bisect import bisect_right
from collections import Counter, defaultdict
from datetime import datetime
import hashlib
import json
from pathlib import Path


OUTPUT_LIMIT = {"max_tokens", "max_output_tokens", "length"}


def timestamp(value):
    return datetime.fromisoformat(value).timestamp()


def complete_rows(path):
    with path.open("rb") as source:
        for line in source:
            if not line.endswith(b"\n"):
                break  # an active writer can be partway through the last record
            yield json.loads(line)


def host_events(path):
    bindings = []
    launches = []
    limits = []
    failures = []
    for row in complete_rows(path):
        item = row["data"].get("Item")
        if not item:
            continue
        data = item["Data"]
        if item["Kind"] == "host_record":
            context = data.get("Context") or {}
            activity = data.get("Activity") or {}
            at = timestamp(item["RecordedAt"])
            if context.get("Kind") == "provider_conversation_binding":
                binding = context["ConversationBinding"]
                bindings.append((at, binding["ID"], binding.get("Boundary", "")))
            if activity.get("Kind") == "claude_startup":
                launches.append(timestamp(activity.get("StartedAt", item["RecordedAt"])))
            if activity.get("Kind") == "claude_stream":
                event = json.loads(activity.get("Reason") or "{}")
                if (event.get("event") == "message_stop" and not event.get("parent_tool_use_id")
                        and event.get("stop_reason") in OUTPUT_LIMIT):
                    limits.append(at)
            if activity.get("Kind") == "model_failure":
                reason = activity.get("Reason", "").lower()
                if "signature" in reason or "redacted_thinking" in reason:
                    failures.append(at)
        if item["Kind"] == "model_response":
            response = data["Response"]
            for output in response.get("Output") or []:
                if output.get("OutputLimit") or response.get("Stop") in OUTPUT_LIMIT:
                    yield "limited_host_item", timestamp(item["RecordedAt"])
    yield "bindings", bindings
    yield "launches", sorted(launches)
    yield "limits", limits
    yield "failures", failures


def fingerprints(message):
    """Use full content block equality, ignoring only the cache marker."""
    content = message.get("content")
    blocks = []
    for block in content if isinstance(content, list) else []:
        if not isinstance(block, dict):
            continue
        kind = block.get("type")
        if kind not in {"thinking", "redacted_thinking", "text"}:
            continue
        if kind == "text" and len(block.get("text", "")) < 64:
            continue  # boilerplate is ambiguous evidence of replay
        body = {key: value for key, value in block.items() if key != "cache_control"}
        digest = hashlib.sha256(json.dumps(body, sort_keys=True).encode()).digest()
        signed = block.get("signature") if kind == "thinking" else block.get("data")
        signature = hashlib.sha256(signed.encode()).digest() if signed else None
        blocks.append((kind, digest, signature))
    return blocks


def recover_blob(blobs, message):
    # A recorded request hash selects a recovered candidate by exact content.
    key = hashlib.sha256(json.dumps(message, ensure_ascii=False, separators=(",", ":")).encode()).hexdigest()
    blobs.setdefault(key, fingerprints(message))


def read_transcript(path):
    blobs = {}
    previous = []
    valid_chain = True
    requests = {}
    usage = {}
    output = defaultdict(set)
    capped = set()
    assistant = defaultdict(dict)
    user = {}
    for row in complete_rows(path):
        kind = row.get("type")
        if kind == "api-request-blob":
            blobs[row["hash"]] = fingerprints(row["message"])
        elif kind == "api-request":
            keep = row["keep"]
            valid_chain = keep == 0 or valid_chain and 0 < keep <= len(previous)
            previous = previous[:keep] + row["tail"]
            if row.get("querySource") != "sdk":
                continue
            requests[row["id"]] = {
                "at": timestamp(row["timestamp"]), "sequence": previous,
                "shape": row["shapeHash"], "valid_chain": valid_chain,
                "model": row.get("params", {}).get("model", ""),
                "markers": row.get("cacheControl") or [],
            }
        elif kind == "assistant" and not row.get("isSidechain"):
            ref = row.get("requestRef")
            if not isinstance(ref, str):
                continue
            message = row["message"]
            use = message.get("usage") or {}
            if use:
                usage[ref] = use  # multiple frames carry the same usage
            output[ref].update(fingerprints(message))
            content = message.get("content")
            if isinstance(content, list):
                group = assistant[ref, message.get("id")]
                position = row.get("apiBlockIndex") or 0
                for index, block in enumerate(content, position):
                    group[index] = block
            if message.get("stop_reason") in OUTPUT_LIMIT:
                capped.add(ref)
        elif kind == "user" and not row.get("isSidechain"):
            content = (row.get("message") or {}).get("content")
            if isinstance(content, str):
                content = [{"type": "text", "text": content}]
            if isinstance(content, list):
                recover_blob(blobs, {"role": "user", "content": content})
                parent = user.get(row.get("parentUuid"))
                if parent:
                    content = parent + content
                    recover_blob(blobs, {"role": "user", "content": content})
                user[row.get("uuid")] = content
    for group in assistant.values():
        recover_blob(blobs, {"role": "assistant", "content": [block for _, block in sorted(group.items())]})
    for ref, request in requests.items():
        request["usage"] = usage.get(ref, {})
        request["complete"] = request["valid_chain"] and all(key in blobs for key in request["sequence"])
        request["blocks"] = {block for key in request["sequence"] for block in blobs.get(key, ())}
        yield request, output.get(ref, ()), ref in capped


def full_prefix(previous, current):
    """Check the preceding message cache breakpoint at message granularity."""
    if previous["shape"] != current["shape"] or previous["model"] != current["model"]:
        return False
    indices = [p["path"][1] for p in previous["markers"] if p["path"][0] == "messages"]
    if not indices:
        return False
    end = max(indices) + 1
    return (previous["valid_chain"] and current["valid_chain"]
            and len(previous["sequence"]) >= end
            and previous["sequence"][:end] == current["sequence"][:end])


def audit(session, transcripts, since, until=float("inf")):
    events = defaultdict(list)
    for name, value in host_events(session):
        if isinstance(value, list):
            events[name].extend(value)
        else:
            events[name].append(value)

    report = {"before": Counter(), "since": Counter()}
    produced_signatures = {}
    capped_blocks = {}
    for index, (bound_at, window_id, boundary) in enumerate(events["bindings"]):
        if bound_at >= until:
            break
        path = transcripts / (window_id + ".jsonl")
        counts = report["since" if bound_at >= since else "before"]
        counts["rotation_bindings" if boundary else "initial_bindings"] += 1
        if not path.is_file():
            counts["missing_transcript"] += 1
            continue
        next_at = events["bindings"][index + 1][0] if index + 1 < len(events["bindings"]) else float("inf")
        seen_launches = set()
        previous = None
        for request, output, capped in read_transcript(path):
            at = request["at"]
            # Bindings may repeat an ID on recovery. Count each request in its own interval.
            if not bound_at <= at < min(next_at, until):
                continue
            counts = report["since" if at >= since else "before"]
            counts["sdk_requests"] += 1
            launch = bisect_right(events["launches"], at) - 1
            resume = launch >= 0 and launch not in seen_launches and previous is not None
            seen_launches.add(launch)
            counts["same_window_launches"] += resume
            first_rotated = previous is None and bool(boundary)
            counts["rotation_first_requests"] += first_rotated
            counts["rotation_first_requests_with_missing_blobs"] += first_rotated and not request["complete"]
            counts["missing_request_blobs"] += not request["complete"]
            counts["invalid_request_deltas"] += not request["valid_chain"]

            signatures = [(digest, signature) for kind, digest, signature in request["blocks"]
                          if kind in {"thinking", "redacted_thinking"} and signature]
            matches = [produced_signatures.get(signature) for digest, signature in signatures]
            counts["requests_with_signatures"] += bool(signatures)
            counts["same_window_launches_with_exact_signed_blocks"] += resume and any(
                prior == (window_id, digest) for (digest, _), prior in zip(signatures, matches))
            counts["rotation_first_requests_with_signatures"] += first_rotated and bool(signatures)
            counts["cross_window_signed_blocks"] += sum(
                prior is not None and prior[0] != window_id for prior in matches)
            counts["changed_signed_blocks"] += sum(
                prior is not None and prior[1] != digest for (digest, _), prior in zip(signatures, matches))
            counts["unmatched_signed_blocks"] += sum(prior is None for prior in matches)
            # Test prior capped output only. Legitimate receipts remain in the history.
            replayed = {kind for kind, digest, _ in request["blocks"] if digest in capped_blocks}
            for kind in replayed:
                counts["capped_replayed_" + kind] += 1
                counts["capped_replayed_on_resume_" + kind] += resume
            for kind, digest, signature in output:
                if signature and kind in {"thinking", "redacted_thinking"}:
                    produced_signatures[signature] = window_id, digest
                if capped:
                    capped_blocks[digest] = window_id
            counts["capped_native_requests"] += capped

            for marker in request["markers"]:
                counts["cache_marks_" + marker["value"].get("ttl", "default")] += 1
            usage = request["usage"]
            if isinstance(usage.get("cache_read_input_tokens"), int):
                counts["usage_requests"] += 1
                counts["cache_read_input_tokens"] += usage["cache_read_input_tokens"]
                counts["direct_input_tokens"] += usage.get("input_tokens") or 0
            for ttl, value in (usage.get("cache_creation") or {}).items():
                counts[ttl] += value
            if previous and full_prefix(previous, request) and usage and previous["usage"]:
                gap = at - previous["at"]
                for seconds in (300, 3600):
                    if gap > seconds:
                        counts[f"eligible_gaps_over_{seconds}s"] += 1
                        counts[f"zero_reads_over_{seconds}s"] += not usage.get("cache_read_input_tokens")
                        prev_usage = previous["usage"]
                        prefix = (prev_usage.get("cache_read_input_tokens", 0)
                                  + prev_usage.get("cache_creation_input_tokens", 0))
                        counts[f"read_below_half_over_{seconds}s"] += usage.get("cache_read_input_tokens", 0) < prefix / 2
                        counts[f"lost_prefix_tokens_over_{seconds}s"] += max(
                            0, prefix - usage.get("cache_read_input_tokens", 0))
            previous = request
    for name in ("limits", "failures", "limited_host_item"):
        for at in events[name]:
            if at < until:
                report["since" if at >= since else "before"][name] += 1
    return report


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("session", type=Path, help="canonical Unreal .session.jsonl")
    parser.add_argument("transcripts", type=Path, help="Claude Code project directory")
    parser.add_argument("--since", required=True, help="UTC RFC3339 cutoff for the report")
    parser.add_argument("--until", help="optional exclusive UTC RFC3339 end of the snapshot")
    args = parser.parse_args()
    until = timestamp(args.until) if args.until else float("inf")
    print(json.dumps(audit(args.session, args.transcripts, timestamp(args.since), until), indent=2))


if __name__ == "__main__":
    main()

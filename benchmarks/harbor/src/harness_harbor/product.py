"""Export product Session receipts to ATIF, with connected child trajectories."""

import hashlib
import json
from collections import defaultdict
from pathlib import Path

from harbor.models.trajectories import Agent, Observation, ObservationResult
from harbor.models.trajectories.subagent_trajectory_ref import SubagentTrajectoryRef

from harness_harbor.pricing import estimate as estimate_offline
from harness_harbor.session_files import session_paths
from harness_harbor.trajectory import convert

FIELDS = {
    "input_tokens": "InputTokens",
    "cache_read_tokens": "CachedInputTokens",
    "cache_write_tokens": "CacheWriteInputTokens",
    "output_tokens": "OutputTokens",
    "reasoning_tokens": "ReasoningTokens",
}
UNKNOWN = {
    "input": "input_tokens",
    "cached_input": "cache_read_tokens",
    "cache_write_input": "cache_write_tokens",
    "output": "output_tokens",
    "reasoning": "reasoning_tokens",
}


def runtime_identity(value):
    while "Provider" not in value and isinstance(value.get("Runtime"), dict):
        value = value["Runtime"]
    return value


def load_session(path):
    items, operations, identity = [], {}, {}
    for number, line in enumerate(path.read_text().splitlines(), 1):
        try:
            envelope = json.loads(line)
        except json.JSONDecodeError:
            raise ValueError(f"Invalid Session JSONL at line {number}") from None
        data = envelope["data"]
        if envelope["type"] == "item":
            item = data["Item"]
            if item["Kind"] == "tool_call_status":
                item["Data"]["Operations"] = data.get("Operations", [])
            if (
                item["Kind"] == "host_record"
                and item["Data"]["Kind"] == "configuration"
            ):
                identity = runtime_identity(item["Data"]["Configuration"])
            items.append(item)
            for op in data.get("Operations", []):
                operations[op["ID"]] = op
        elif envelope["type"] == "operation":
            op = data["Operation"]
            operations[op["ID"]] = op
    children = []
    for op in operations.values():
        plan = op.get("State", {}).get("Plan", {})
        value = plan.get("Data", {})
        if plan.get("Type") == "subagent" and value.get("Action") == "start":
            children.append(
                {
                    "session_id": value.get("ChildID")
                    or op.get("State", {}).get("Handle", {}).get("ChildID")
                    or "child-"
                    + hashlib.sha256(
                        (
                            "unreal-agent/subagent/v1\0"
                            + value["ParentID"]
                            + "\0"
                            + op["ID"]
                        ).encode()
                    ).hexdigest()[:32],
                    "operation_id": op["ID"],
                    "template": value["Template"],
                }
            )
    return items, identity, children


def usage_rows(items, identity):
    provider = identity.get("Provider", {}).get("provider", "unknown")
    model = identity.get("Provider", {}).get("model", {}).get("id", "unknown")
    authoritative = defaultdict(list)
    executions, selections, stream_models = {}, {}, {}
    origins = {}
    stream_outputs = defaultdict(lambda: defaultdict(int))
    receipts = []
    for item in items:
        data = item["Data"]
        if item["Kind"] == "turn":
            origins[data["ID"]] = data.get("ToolContinuation") or data["ID"]
        if item["Kind"] == "host_record" and data["Kind"] == "runtime_applied":
            provider, model = data["Selection"]["Provider"], data["Selection"]["Model"]
        if item["Kind"] == "model_response":
            receipts.append((data["TurnID"], provider, model, data["Response"]))
        if item["Kind"] == "host_record":
            activity = data.get("Activity", {})
            kind, turn = activity.get("Kind"), activity.get("ID")
            if kind in {"provider_usage", "claude_usage"} and "Usage" in activity:
                selections[turn] = provider, model
                if kind == "claude_usage":
                    executions[turn] = activity
                    continue
                # Keep the turn even if its only row is synthetic, so a
                # tool-boundary receipt cannot be counted a second time.
                values = authoritative[turn]
                if activity.get("Model") == "<synthetic>":
                    continue
                values.append(
                    (
                        activity.get("Provider", provider),
                        activity.get("Model") or model,
                        activity["Usage"],
                    )
                )
            elif kind == "claude_stream":
                # E3b Session observations repair older per-model output
                # rows too. No CLI transcript or raw provider data is read.
                observation = json.loads(activity["Reason"])
                turn = turn.split("/stream/", 1)[0]
                name = activity.get("Model")
                if not name or name == "<synthetic>":
                    continue
                if not observation.get("parent_tool_use_id"):
                    stream_models[turn] = name
                if observation["event"] == "message_stop":
                    output = observation.get("output_tokens")
                    if output is not None:
                        stream_outputs[turn][name] += output
    rows = []
    for turn, provider, model, response in receipts:
        origin = origins.get(turn, turn)
        if origin not in authoritative and origin not in executions:
            name = response.get("Model") or model
            if name != "<synthetic>":
                rows.append((provider, name, response["Usage"]))
    for turn, execution in executions.items():
        provider, model = selections[turn]
        provider = execution.get("Provider", provider)
        values = authoritative[turn]
        names = {name for _, name, _ in values}
        model = (
            next(iter(names))
            if len(names) == 1
            else execution.get("Model") or stream_models.get(turn) or model
        )
        # Full execution usage replaces both same-turn receipts and model
        # fragments. Preserve other native models and assign the remainder
        # to the main model, without counting the execution total twice.
        main, others = dict(execution["Usage"]), []
        unknown = set(main.get("Unknown", []))
        for connection, name, usage in values:
            if connection == provider and name == model:
                continue
            usage = dict(usage)
            if name in stream_outputs[turn]:
                usage["OutputTokens"] = stream_outputs[turn][name]
            others.append((connection, name, usage))
            for field in FIELDS.values():
                main[field] = main.get(field, 0) - usage.get(field, 0)
            unknown.update(usage.get("Unknown", []))
        main["Unknown"] = sorted(unknown)
        for field in unknown:
            if field in UNKNOWN:
                main[FIELDS[UNKNOWN[field]]] = 0
        authoritative[turn] = [(provider, model, main), *others]
    for values in authoritative.values():
        rows.extend(values)
    return rows


def estimate(provider, model, usage, *, prices=None):
    if prices is not None:
        return estimate_offline(provider, model, usage, prices=prices)
    if any(
        f in usage.get("Unknown", [])
        for f in ("input", "cached_input", "cache_write_input", "output")
    ):
        return None
    import litellm

    prefix = "anthropic" if provider == "claude-api" else provider
    entry = next(
        (m for m in (f"{prefix}/{model}", model) if m in litellm.model_cost), None
    )
    if entry is None:
        return None
    try:
        costs = litellm.cost_per_token(
            model=entry,
            prompt_tokens=usage.get("InputTokens", 0),
            completion_tokens=usage.get("OutputTokens", 0),
            cache_read_input_tokens=usage.get("CachedInputTokens", 0),
            cache_creation_input_tokens=usage.get("CacheWriteInputTokens", 0),
        )
    except Exception:
        return None
    return sum(costs)


def aggregate(rows, *, prices=None):
    by_connection = {}
    for provider, model, usage in rows:
        if model == "<synthetic>":
            continue
        connection = by_connection.setdefault(provider, {"models": {}})
        counts = connection["models"].setdefault(
            model,
            {**dict.fromkeys(FIELDS, 0), "cost_usd": 0.0, "unknown_usage_fields": []},
        )
        for public, field in FIELDS.items():
            counts[public] += usage.get(field, 0)
        counts["unknown_usage_fields"] = sorted(
            set(counts["unknown_usage_fields"]) | set(usage.get("Unknown", []))
        )
        cost = estimate(provider, model, usage, prices=prices)
        counts["cost_usd"] = (
            None
            if counts["cost_usd"] is None or cost is None
            else counts["cost_usd"] + cost
        )
    for connection in by_connection.values():
        for counts in connection["models"].values():
            for field in counts["unknown_usage_fields"]:
                if field in UNKNOWN:
                    counts[UNKNOWN[field]] = None
        connection.update(sum_counts(connection["models"].values()))
    return by_connection, sum_counts(by_connection.values())


def sum_counts(values):
    values = list(values)
    result = {field: sum((v.get(field) or 0) for v in values) for field in FIELDS}
    unknown = sorted(
        {field for value in values for field in value["unknown_usage_fields"]}
        if values
        else UNKNOWN.keys()
    )
    for field in unknown:
        if field in UNKNOWN:
            result[UNKNOWN[field]] = None
    result["unknown_usage_fields"] = unknown
    result["cost_usd"] = (
        sum(v["cost_usd"] for v in values)
        if values and all(v["cost_usd"] is not None for v in values)
        else None
    )
    return result


def export_product(directory: Path, agent: Agent, session_id: str):
    sessions = {}
    for sid, path in session_paths(directory / "sessions").items():
        items, identity, children = load_session(path)
        child_agent = agent.model_copy(deep=True)
        provider = identity.get("Provider", {}).get("provider", "unknown")
        model = identity.get("Provider", {}).get("model", {}).get("id", "unknown")
        child_agent.model_name = f"{provider}/{model}"
        trajectory = convert(
            (json.dumps(i) for i in items),
            child_agent,
            sid,
            output_dir=directory,
            product=True,
        )
        sessions[sid] = (trajectory, usage_rows(items, identity), children)

    def finish(sid):
        trajectory, rows, children = sessions[sid]
        rows = list(rows)
        references = []
        for child in children:
            target = child["session_id"]
            path = f"trajectory.{target}.json"
            references.append(
                {**child, "trajectory_path": path, "missing": target not in sessions}
            )
            if target not in sessions:
                continue
            child_rows = finish(target)
            rows.extend(child_rows)
            ref = SubagentTrajectoryRef(session_id=target, trajectory_path=path)
            # CLI starts are canonical Operations even when no model tool-call
            # ID exists. Keep that provenance on a system observation.
            if trajectory.steps:
                step = trajectory.steps[-1]
                if step.observation is None:
                    step.observation = Observation(results=[])
                step.observation.results.append(
                    ObservationResult(
                        content=f"Unreal child: {child['template']}",
                        subagent_trajectory_ref=[ref],
                        extra={"operation_id": child["operation_id"]},
                    )
                )
        breakdown, total = aggregate(rows)
        trajectory.final_metrics.total_prompt_tokens = total["input_tokens"]
        trajectory.final_metrics.total_completion_tokens = total["output_tokens"]
        trajectory.final_metrics.total_cached_tokens = total["cache_read_tokens"]
        trajectory.final_metrics.total_cost_usd = total["cost_usd"]
        trajectory.final_metrics.extra = {"usage_by_connection": breakdown, **total}
        trajectory.extra.update(
            {"children": references, "usage_by_connection": breakdown}
        )
        trajectory.notes = (
            "Product Host audit. Root totals include each connected child once. "
            "Execution totals replace same-turn provider and receipt usage; "
            "synthetic models are excluded. "
            "Unknown usage is marked. Costs use exact LiteLLM map entries or null. "
            "Cache reads/writes are subsets of input; reasoning is a subset of output. "
            "Native Agent message prefixes link to their parent Agent tool call. "
            "Sanitized exports are not executable Session/replay data."
        )
        destination = (
            "trajectory.json" if sid == session_id else f"trajectory.{sid}.json"
        )
        (directory / destination).write_text(
            json.dumps(trajectory.to_json_dict(), ensure_ascii=False, indent=2) + "\n"
        )
        return rows

    finish(session_id)
    return sessions[session_id][0]

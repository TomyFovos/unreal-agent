import base64
import hashlib
import json
from collections.abc import Iterable
from pathlib import Path
from typing import Any

from harbor.models.trajectories import (
    Agent,
    ContentPart,
    FinalMetrics,
    ImageSource,
    Metrics,
    Observation,
    ObservationResult,
    Step,
    ToolCall,
    Trajectory,
)

from harness_harbor.remote_results import (
    REMOTE_PENDING,
    ast_result,
    dap_result,
    lsp_result,
    native_result,
    subagent_result,
)

RUNNING = (
    "Tool call is still running. Its result arrives in a later turn: "
    "continue with independent work, or end your turn to wait for it."
)
TERMINAL = {"completed", "failed", "canceled"}
USAGE_FIELDS = {
    "prompt": "InputTokens",
    "completion": "OutputTokens",
    "cached": "CachedInputTokens",
    "reasoning": "ReasoningTokens",
    "cache_write": "CacheWriteInputTokens",
}


def bash_result(data: dict[str, Any]) -> str:
    status = data["Status"]
    operations = {op["ID"]: op for op in data.get("Operations", [])}
    waiting = status.get("WaitingFor") or []
    if status.get("Error"):
        if waiting or operations:
            raise ValueError("Bash call has both a validation error and operations")
        return "Error: " + status["Error"]
    if len(waiting) != 1:
        raise ValueError(f"Bash call has {len(waiting)} operations, want 1")
    op = operations[waiting[0]]
    if op["Type"] != "shell":
        raise ValueError(f"Unsupported operation type: {op['Type']}")
    state = op["State"]
    if op["Status"] in {"ready", "awaiting", "canceling"}:
        return RUNNING
    if op["Status"] not in TERMINAL:
        raise ValueError(f"Unsupported operation status: {op['Status']}")

    error = state.get("TerminalError") or ""
    if not error and op["Status"] in {"failed", "canceled"}:
        error = "shell operation " + op["Status"]
    parts = []
    value = state.get("Result")
    if value is not None:
        stdout, stderr = value["Out"], value["Err"]
        if not isinstance(stdout, str) or not isinstance(stderr, str):
            raise ValueError("Expected current runner text output")
        if stdout:
            parts.append(stdout)
        if stderr:
            parts.append("Stderr:\n" + stderr)
        if value["ExitCode"] != 0:
            parts.append(f"Exit code: {value['ExitCode']}")
    elif op["Status"] == "completed":
        raise ValueError("Completed shell operation has no result")
    else:
        if state.get("OutPath"):
            parts.append("Stdout capture: " + state["OutPath"])
        if state.get("ErrPath"):
            parts.append("Stderr capture: " + state["ErrPath"])
    if error:
        parts.append("Error: " + error)
    return "\n".join(parts) if parts else "(no output)"


def view_image_result(
    data: dict[str, Any], output_dir: Path | None
) -> str | list[ContentPart]:
    status = data["Status"]
    operations = {op["ID"]: op for op in data.get("Operations", [])}
    waiting = status.get("WaitingFor") or []
    if status.get("Error"):
        if waiting or operations:
            raise ValueError(
                "ViewImage call has both a validation error and operations"
            )
        return "Error: " + status["Error"]
    if len(waiting) != 1:
        raise ValueError(f"ViewImage call has {len(waiting)} operations, want 1")
    op = operations[waiting[0]]
    if op["Type"] != "view_image":
        raise ValueError(f"Unsupported operation type: {op['Type']}")
    if op["Status"] in {"ready", "awaiting", "canceling"}:
        return RUNNING
    if op["Status"] not in TERMINAL:
        raise ValueError(f"Unsupported operation status: {op['Status']}")

    failed = op["Status"] != "completed"
    value = op["State"].get("Result") or {}
    details = []
    if failed:
        error = value.get("Error") or "view-image operation " + op["Status"]
        details.append("Error: " + error)
    elif (
        not value.get("Content")
        or value.get("EncodedMIMEType") not in {"image/jpeg", "image/png"}
        or not 0 < value.get("ScaleRatio", 0) <= 1
        or value.get("Error")
    ):
        raise ValueError("Completed ViewImage operation has an invalid image result")

    original = value.get("OriginalMIMEType")
    if original and (failed or original != value.get("EncodedMIMEType")):
        details.append("original MIME type: " + original)
    width, height = value.get("OriginalWidth", 0), value.get("OriginalHeight", 0)
    ratio = value.get("ScaleRatio", 1)
    if width > 0 and height > 0 and (failed or ratio < 1):
        details.append(f"original dimensions: {width}x{height}")
    if failed:
        return "; ".join(details)
    if ratio < 1:
        details.append(
            f"multiply coordinates by {1 / ratio:.2f} to approximate original"
        )

    if output_dir is None:
        raise ValueError("ViewImage observations require an output directory")
    image = base64.b64decode(value["Content"], validate=True)
    extension = "jpg" if value["EncodedMIMEType"] == "image/jpeg" else "png"
    relative = Path("images") / f"{hashlib.sha256(image).hexdigest()}.{extension}"
    path = output_dir / relative
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(image)
    content = [
        ContentPart(
            type="image",
            source=ImageSource(
                media_type=value["EncodedMIMEType"], path=relative.as_posix()
            ),
        )
    ]
    if details:
        content.append(ContentPart(type="text", text="; ".join(details)))
    return content


def convert(
    lines: Iterable[str],
    agent: Agent,
    session_id: str,
    *,
    output_dir: Path | None = None,
    product: bool = False,
) -> Trajectory:
    steps: list[Step] = []
    calls: dict[str, tuple[Step, str]] = {}
    pending_observations: list[ObservationResult] = []
    errors: list[str] = []
    host_records: list[dict] = []
    runner_records: list[dict] = []
    changed_files: set[str] = set()
    session_end: dict | None = None
    totals = dict(prompt=0, completion=0, cached=0, reasoning=0, cache_write=0)
    previous_sequence = 0
    for line_number, line in enumerate(lines, 1):
        if not line.strip():
            continue
        try:
            item = json.loads(line)
        except json.JSONDecodeError as exc:
            raise ValueError(f"Invalid runner JSONL at line {line_number}") from exc
        if item.get("type") == "error":
            errors.append(item["message"])
            continue
        if item.get("type") == "session_end":
            session_end = {"IsError": item["is_error"]}
            continue
        sequence = item["Sequence"]
        if sequence <= previous_sequence:
            raise ValueError("Runner records must have increasing sequence numbers")
        previous_sequence = sequence
        kind, data = item["Kind"], item["Data"]
        timestamp = item["RecordedAt"]
        extra = {"sequence": sequence}
        child_id = item.get("ChildID", "")
        if child_id:
            extra["child_id"] = child_id
        if "SourceSequence" in item:
            extra["source_sequence"] = item["SourceSequence"]
        if kind == "host_record":
            if data.get("Version") != 1 or (
                not product
                and data.get("Kind")
                not in (
                    "configuration",
                    "stop_complete",
                    "project_instructions",
                    "activity",
                    "context",
                )
            ):
                raise ValueError("Unsupported Host metadata record")
            host_records.append({**extra, "timestamp": timestamp, "data": data})
            context = data.get("Context", {})
            for usage in (
                context.get("Usage"),
                context.get("Conversation", {}).get("Usage"),
            ):
                if usage:
                    for key, field in USAGE_FIELDS.items():
                        totals[key] += usage.get(field, 0)
        elif kind in {"subagent_start", "subagent_end", "file_change", "session_end"}:
            runner_records.append(
                {**extra, "timestamp": timestamp, "kind": kind, "data": data}
            )
            if kind == "file_change":
                changed_files.add(data["Path"])
            elif kind == "session_end":
                session_end = data
                changed_files.update(data.get("ChangedFiles", []))
        elif kind == "turn":
            for result in pending_observations[:]:
                if result.extra.get("child_id", "") == child_id:
                    result.extra["available_before_turn"] = data["ID"]
                    pending_observations.remove(result)
        elif kind == "input":
            if data["Kind"] != "external":
                continue
            if not isinstance(data["Payload"], str):
                raise ValueError("External runner input must be text")
            steps.append(
                Step(
                    step_id=len(steps) + 1,
                    source="user",
                    message=data["Payload"],
                    timestamp=timestamp,
                    extra=extra,
                )
            )
        elif kind == "model_response":
            response = data["Response"]
            texts, reasoning, tool_calls = [], [], []
            for output in response.get("Output", []):
                value = output["Data"]
                match output["Type"]:
                    case "message":
                        texts.append(value["Text"])
                    case "reasoning":
                        reasoning.extend(value.get("Summary", []))
                    case "tool_call":
                        raw = value["Arguments"]
                        try:
                            arguments = json.loads(raw)
                        except json.JSONDecodeError:
                            arguments = {"raw_arguments": raw}
                        if not isinstance(arguments, dict):
                            arguments = {"raw_arguments": raw}
                        tool_calls.append(
                            ToolCall(
                                tool_call_id=(
                                    f"{child_id}/{value['CallID']}"
                                    if child_id
                                    else value["CallID"]
                                ),
                                function_name=value["Name"],
                                arguments=arguments,
                            )
                        )
                    case _:
                        raise ValueError(f"Unsupported model output: {output['Type']}")
            usage = response["Usage"]
            counts = {key: usage[field] for key, field in USAGE_FIELDS.items()}
            for key, count in counts.items():
                totals[key] += count
            step = Step(
                step_id=len(steps) + 1,
                source="agent",
                timestamp=timestamp,
                message="\n\n".join(texts),
                reasoning_content="\n\n".join(reasoning) or None,
                model_name=response.get("Model") or agent.model_name,
                tool_calls=tool_calls or None,
                metrics=Metrics(
                    prompt_tokens=counts["prompt"],
                    completion_tokens=counts["completion"],
                    cached_tokens=counts["cached"],
                    extra={
                        "reasoning_tokens": counts["reasoning"],
                        "cache_write_tokens": counts["cache_write"],
                        "unknown_usage_fields": usage.get("Unknown", []),
                    },
                ),
                llm_call_count=1,
                extra={
                    **extra,
                    "turn_id": data["TurnID"],
                    "stop": response["Stop"],
                    "failure": response.get("Failure"),
                },
            )
            steps.append(step)
            for call in tool_calls:
                if call.tool_call_id in calls:
                    raise ValueError(f"Duplicate tool call: {call.tool_call_id}")
                calls[call.tool_call_id] = step, call.function_name
        elif kind == "tool_call_status":
            call_id = f"{child_id}/{data['CallID']}" if child_id else data["CallID"]
            step, name = calls[call_id]
            if name == "Bash":
                content = bash_result(data)
            elif name == "ViewImage":
                content = view_image_result(data, output_dir)
            elif name in {
                "Read",
                "Write",
                "Edit",
                "Grep",
                "Glob",
                "read",
                "write",
                "edit",
                "grep",
                "glob",
            }:
                content = native_result(data)
            elif name in {"ast_grep", "ast_edit"}:
                content = ast_result(data)
            elif name == "LSP":
                content = lsp_result(data)
            elif name == "DAP":
                content = dap_result(data)
            elif name in {
                "SubagentStart",
                "SubagentSend",
                "SubagentCancel",
                "SubagentApprove",
                "SendParent",
                "Finish",
            }:
                content = subagent_result(data)
            elif product and name == "claude_code":
                values = [
                    op.get("State", {}).get("Value", {})
                    for op in data.get("Operations", [])
                ]
                content = data["Status"].get("Error") or "\n".join(
                    v.get("content", "") for v in values
                )
            elif product:
                content = data["Status"].get("Error") or "\n".join(
                    op.get("State", {}).get("TerminalResult")
                    or op.get("State", {}).get("TerminalError")
                    or json.dumps(op.get("State", {}), ensure_ascii=False)
                    for op in data.get("Operations", [])
                )
            elif data["Status"].get("Error") and not data.get("Operations"):
                content = data["Status"]["Error"]
            else:
                raise ValueError(f"Unsupported tool result: {name}")
            pending_observations = [
                result
                for result in pending_observations
                if not (
                    result.source_call_id == call_id
                    and result.content in (RUNNING, *REMOTE_PENDING)
                )
            ]
            result = ObservationResult(
                source_call_id=call_id,
                content=content,
                extra={**extra, "timestamp": timestamp},
            )
            if step.observation is None:
                step.observation = Observation(results=[])
            step.observation.results.append(result)
            pending_observations.append(result)
        else:
            raise ValueError(f"Unsupported runner record: {kind}")
    if session_end is not None:
        if session_end["IsError"]:
            if not errors:
                errors.append("Runner session ended with an error")
        elif "Usage" in session_end:
            # The terminal summary already includes parent and descendant usage.
            # Replace the streamed total rather than count those tokens again.
            totals = {
                key: session_end["Usage"][field] for key, field in USAGE_FIELDS.items()
            }
    return Trajectory(
        schema_version="ATIF-v1.7",
        session_id=session_id,
        agent=agent,
        steps=steps,
        notes=(
            "Observations are attached to their originating tool calls. "
            "They may arrive asynchronously after later agent steps; use sequence, "
            "timestamp and available_before_turn metadata for ordering. "
            "This is a session audit, not a complete provider-request replay. "
            "Costs are unknown: runner usage contains tokens, not billed amounts."
        ),
        final_metrics=FinalMetrics(
            total_prompt_tokens=totals["prompt"],
            total_completion_tokens=totals["completion"],
            total_cached_tokens=totals["cached"],
            total_steps=len(steps),
            extra={
                "reasoning_tokens": totals["reasoning"],
                "cache_write_tokens": totals["cache_write"],
            },
        ),
        extra={
            "runner_errors": errors,
            "host_records": host_records,
            "runner_records": runner_records,
            "changed_files": sorted(changed_files),
            "session_end": session_end,
        },
    )

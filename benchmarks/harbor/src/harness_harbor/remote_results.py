"""Render persisted typed remote-job results as their tool translators do.

Handle is the structured result, not a process handle or truncated display text.
Never substitute TerminalResult (which may be only a code or truncated bytes).
"""

import json
from typing import Any

NATIVE_PENDING = "File operation is pending."


def permission_text(denial: dict[str, Any]) -> str:
    return f"permission {denial['Code']}: {denial['Capability']}: {denial['Reason']}"


def one_operation(data: dict[str, Any], name: str) -> dict[str, Any]:
    operations = data.get("Operations") or []
    waiting = data["Status"].get("WaitingFor") or []
    if len(operations) != 1 or len(waiting) != 1 or operations[0]["ID"] != waiting[0]:
        raise ValueError(f"{name} result requires one matching operation")
    return operations[0]


def remote_state(op: dict[str, Any], plan_type: str) -> dict[str, Any]:
    if op["Type"] != "remote_job" or op.get("Version") != 2:
        raise ValueError("Unsupported remote-job operation type/version")
    state = op["State"]
    plan = state["Plan"]
    if plan["Type"] != plan_type or plan.get("Version") != 1:
        raise ValueError("Unsupported remote-job plan type/version")
    return state


def typed_json(value: Any) -> str:
    if not isinstance(value, dict) or type(value.get("version")) is not int:
        raise ValueError("Invalid typed remote-job result")
    if value["version"] != 1:
        raise ValueError("Unsupported typed remote-job result version")
    return json.dumps(value, ensure_ascii=False, separators=(",", ":"), allow_nan=False)


def native_result(data: dict[str, Any]) -> str:
    # Match harness/tool/native.TranslateResult, including denial precedence and
    # its pending text. A typed failure is JSON too, not a synthetic shell error.
    operations = data.get("Operations") or []
    if len(operations) == 1 and operations[0].get("Denial"):
        return permission_text(operations[0]["Denial"])
    if data["Status"].get("Error"):
        return data["Status"]["Error"]
    op = one_operation(data, "native")
    state = remote_state(op, "native_file")
    if "Handle" not in state:
        return NATIVE_PENDING
    return typed_json(state["Handle"])

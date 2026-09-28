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


AST_PENDING = "Structural operation is pending."
LSP_PENDING = '{"version":1,"code":"running"}'
DAP_PENDING = "Debugger operation is running."
REMOTE_PENDING = (NATIVE_PENDING, AST_PENDING, LSP_PENDING, DAP_PENDING)


def ast_result(data: dict[str, Any]) -> str:
    operations = data.get("Operations") or []
    if len(operations) == 1 and operations[0].get("Denial"):
        return permission_text(operations[0]["Denial"])
    if data["Status"].get("Error"):
        return data["Status"]["Error"]
    state = remote_state(one_operation(data, "AST"), "structural_code")
    if "Handle" not in state:
        return AST_PENDING
    return typed_json(state["Handle"])


def lsp_result(data: dict[str, Any]) -> str:
    if data["Status"].get("Error"):
        return data["Status"]["Error"]
    op = one_operation(data, "LSP")
    if op.get("Denial"):
        denial = op["Denial"]
        return typed_json({"version": 1, "code": denial["Code"], "denial": denial})
    state = remote_state(op, "lsp")
    if "Handle" in state:
        return typed_json(state["Handle"])
    return state.get("TerminalError") or LSP_PENDING


def dap_result(data: dict[str, Any]) -> str:
    if data["Status"].get("Denial"):
        return permission_text(data["Status"]["Denial"])
    if data["Status"].get("Error"):
        return data["Status"]["Error"]
    op = one_operation(data, "DAP")
    if op.get("Denial"):
        return permission_text(op["Denial"])
    state = remote_state(op, "dap")
    if op["Status"] in {"ready", "awaiting", "canceling"}:
        return DAP_PENDING
    handle = state.get("Handle")
    if not isinstance(handle, dict):
        raise ValueError("Invalid typed DAP result")
    known = {
        "version",
        "handle",
        "command",
        "status",
        "stop_epoch",
        "items",
        "value",
        "variables_reference",
        "truncated",
        "error",
    }
    if handle.keys() - known:
        raise ValueError("Invalid typed DAP result fields")
    encoded = typed_json(handle)
    if len(encoded.encode("utf-8")) > 32 << 10:
        raise ValueError("Typed DAP result exceeds limit")
    return encoded

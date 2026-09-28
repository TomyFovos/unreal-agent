import copy
import json
import unittest

from harbor.models.trajectories import Agent, Trajectory

from harness_harbor.remote_results import (
    AST_PENDING,
    DAP_PENDING,
    LSP_PENDING,
    ast_result,
    dap_result,
    lsp_result,
    native_result,
)
from harness_harbor.trajectory import convert


def status(handle=None, state="completed", plan="native_file"):
    value = {"Plan": {"Type": plan, "Version": 1, "Data": {}}}
    if handle is not None:
        value["Handle"] = handle
    return {
        "CallID": "call",
        "Status": {"WaitingFor": ["op"]},
        "Operations": [{
            "ID": "op", "Type": "remote_job", "Version": 2,
            "Status": state, "State": value,
        }],
    }


def record(sequence, kind, data):
    return json.dumps({
        "Sequence": sequence, "Kind": kind, "Data": data,
        "RecordedAt": "2026-09-28T10:00:00Z",
    })


def model_call(name):
    return {
        "TurnID": "turn",
        "Response": {
            "ID": "response", "Stop": "complete",
            "Output": [{
                "Type": "tool_call",
                "Data": {"CallID": "call", "Name": name, "Arguments": "{}"},
            }],
            "Usage": {
                "InputTokens": 10, "OutputTokens": 2, "CachedInputTokens": 0,
                "CacheWriteInputTokens": 0, "ReasoningTokens": 0,
            },
        },
    }


class NativeResultTests(unittest.TestCase):
    def test_all_native_tools_convert_typed_success_and_failures(self):
        for name in ("Read", "Write", "Edit", "Grep", "Glob"):
            for code in ("ok", "applied", "binary", "no_match", "stale", "canceled"):
                with self.subTest(name=name, code=code):
                    handle = {
                        "version": 1, "code": code, "path": "source.txt",
                        "content": 'literal: aGVsbG8= \\n "text"',
                        "truncated": True,
                        "revision": {"exists": True, "sha256": "abc"},
                        "mutation": {"version": 1, "code": code},
                    }
                    data = status(handle)
                    data["Operations"][0]["State"]["TerminalResult"] = "truncated..."
                    self.assertEqual(json.loads(native_result(data)), handle)
                    trajectory = convert([
                        record(1, "model_response", model_call(name)),
                        record(2, "tool_call_status", data),
                    ], Agent(name="unreal-agent", version="test"), "session")
                    output = trajectory.steps[0].observation.results[0].content
                    self.assertEqual(json.loads(output), handle)
                    Trajectory.model_validate(trajectory.to_json_dict())

    def test_pending_validation_and_permission_match_translator(self):
        self.assertEqual(native_result(status(state="awaiting")),
                         "File operation is pending.")
        self.assertEqual(native_result({"Status": {"Error": "invalid arguments"}}),
                         "invalid arguments")
        data = status({"version": 1, "code": "never display this"})
        data["Status"]["Error"] = "earlier error"
        data["Operations"][0]["Denial"] = {
            "Code": "denied", "Capability": "file write", "Reason": "outside roots",
        }
        self.assertEqual(native_result(data),
                         "permission denied: file write: outside roots")

    def test_only_latest_pending_native_result_is_available(self):
        trajectory = convert([
            record(1, "model_response", model_call("Read")),
            record(2, "tool_call_status", status(state="ready")),
            record(3, "tool_call_status", status(state="awaiting")),
            record(4, "turn", {"ID": "next"}),
        ], Agent(name="unreal-agent", version="test"), "session")
        results = trajectory.steps[0].observation.results
        self.assertNotIn("available_before_turn", results[0].extra)
        self.assertEqual(results[1].extra["available_before_turn"], "next")

    def test_invalid_typed_versions_and_mismatched_operation_are_rejected(self):
        original = status({"version": 1, "code": "ok"})
        cases = []
        for field, value in (("Version", 1), ("Type", "shell"), ("ID", "other")):
            data = copy.deepcopy(original)
            data["Operations"][0][field] = value
            cases.append(data)
        for handle in ({"version": 2}, {"version": True}, "not a typed object"):
            cases.append(status(handle))
        cases.append(status({"version": 1}, plan="other"))
        for data in cases:
            with self.subTest(data=data), self.assertRaises(ValueError):
                native_result(data)



class ExtendedResultTests(unittest.TestCase):
    def test_typed_tools_preserve_result_data(self):
        cases = [
            ("ast_grep", "structural_code", ast_result,
             {"version": 1, "code": "ok", "count": 2, "files": [
                 {"path": "source.go", "matches": [
                     {"start_byte": 2, "end_byte": 3, "text": "x"}
                 ]}
             ]}),
            ("ast_edit", "structural_code", ast_result,
             {"version": 1, "code": "applied", "count": 2,
              "mutation": {"version": 1, "code": "applied"}}),
            ("LSP", "lsp", lsp_result,
             {"version": 1, "code": "ok", "generation": "server-generation",
              "encoding": "utf-16", "data": [{"uri": "file:///source.go"}]}),
            ("DAP", "dap", dap_result,
             {"version": 1, "handle": {"id": "debug", "generation": "current"},
              "command": "variables", "status": "stopped", "stop_epoch": 7,
              "items": [{"name": "x", "value": "3"}], "truncated": True}),
        ]
        for name, plan, render, handle in cases:
            with self.subTest(name=name):
                data = status(handle, plan=plan)
                data["Operations"][0]["State"]["TerminalResult"] = "code only"
                self.assertEqual(json.loads(render(data)), handle)
                result = convert([
                    record(1, "model_response", model_call(name)),
                    record(2, "tool_call_status", data),
                ], Agent(name="unreal-agent", version="test"), "session")
                self.assertEqual(
                    json.loads(result.steps[0].observation.results[0].content),
                    handle,
                )
                Trajectory.model_validate(result.to_json_dict())

    def test_pending_and_errors_follow_each_translator(self):
        for plan, render, pending in (
            ("structural_code", ast_result, AST_PENDING),
            ("lsp", lsp_result, LSP_PENDING),
            ("dap", dap_result, DAP_PENDING),
        ):
            with self.subTest(plan=plan):
                self.assertEqual(render(status(state="ready", plan=plan)), pending)
                self.assertEqual(render({"Status": {"Error": "invalid"}}), "invalid")
        data = status(state="failed", plan="lsp")
        data["Operations"][0]["State"]["TerminalError"] = "server disconnected"
        self.assertEqual(lsp_result(data), "server disconnected")

    def test_denial_representation_matches_each_translator(self):
        denial = {"Code": "unsupported", "Capability": "process",
                  "Reason": "sandbox unavailable"}
        for plan, render in (("structural_code", ast_result), ("dap", dap_result)):
            data = status(plan=plan)
            data["Operations"][0]["Denial"] = denial
            self.assertEqual(render(data),
                             "permission unsupported: process: sandbox unavailable")
        data = status(plan="lsp")
        data["Operations"][0]["Denial"] = denial
        self.assertEqual(json.loads(lsp_result(data)),
                         {"version": 1, "code": "unsupported", "denial": denial})

    def test_invalid_dap_payload_and_plan_versions_are_rejected(self):
        for handle in ({"version": 2}, {"version": 1, "unknown": 2},
                       {"version": 1, "value": "x" * (33 << 10)}):
            with self.assertRaises(ValueError):
                dap_result(status(handle, plan="dap"))
        for render in (ast_result, lsp_result, dap_result):
            with self.assertRaises(ValueError):
                render(status({"version": 1}, plan="unrecognized"))


if __name__ == "__main__":
    unittest.main()

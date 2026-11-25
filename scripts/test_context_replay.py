from contextlib import redirect_stdout
import csv
from io import StringIO
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

from context_replay import (
    CACHES, Profile, Settings, Step, fitted_projection, main, predict_cache, read_context_policies, replay, ttl_report,
)
from context_replay_native import audit
from context_replay_records import Request, tool_actions


def request(number, estimate):
    return Request("claude", "opus", "", number * 10, number * 10 + 5, number, estimate,
                   estimate - 2, 0, 1, {"EstimatedInputTokens": estimate}, None,
                   "header", 0, 0, 0, f"invocation-{number}", "test")


def profile():
    return Profile([], 1, 100, [(0, 10)], 1, 2, {("", True): 1000}, 2, 600)


class ReplayTest(unittest.TestCase):
    def test_cache_stays_warm_through_ttl_and_reuses_header_on_rotation(self):
        step = Step(request(1, 8000), 0, 0, 8000, 10_000, False, False, head_gap=200)
        cache = CACHES["claude"]
        hot = predict_cache(step, profile(), cache, 8000, 7000, 300, False, ttl=300)
        self.assertEqual((hot.read, hot.write, hot.uncached), (6998, 1000, 2))
        expired = predict_cache(step, profile(), cache, 8000, 7000, 301, False, ttl=300)
        self.assertEqual((expired.read, expired.write, expired.uncached), (0, 7998, 2))
        rotated = predict_cache(step, profile(), cache, 8000, 7000, 200, True, ttl=300)
        self.assertEqual((rotated.read, rotated.write, rotated.uncached), (1000, 6998, 2))
        self.assertGreater(expired.cost(cache), hot.cost(cache))

    def test_growth_trigger_allows_another_window_of_growth(self):
        rows = [[Step(request(1, 1000), 1000, 1000, 1000, 10_000, False, False, l2_demand=10),
                 Step(request(2, 1600), 600, 600, 1600, 10_000, False, False, l2_demand=10)]]
        profiles = {None: profile()}
        growth = replay(rows, profiles, CACHES["claude"], Settings(1000, 0.5, 10))
        total = replay(rows, profiles, CACHES["claude"], Settings(1000, 0.5, 10, trigger="total"))
        self.assertEqual((growth.rebuilds, total.rebuilds), (1, 2))
        self.assertEqual(growth.predicted["input"], 1600)
        self.assertEqual(total.predicted["input"], 1000)

    def test_ttl_schedule_pays_for_pings_before_unexpired_requests(self):
        rows = []
        for i, at in enumerate((10, 290, 600), 1):
            req = request(i, 8000)
            req.at, req.end = at, at + 5
            rows.append(Step(req, 0, 0, 8000, 10_000, False, False))
        result = ttl_report([rows], {None: profile()}, CACHES["claude"], 0)["time_scenarios"][300]
        self.assertEqual(result["simulated_expiries"], 1)
        self.assertEqual(result["hour_bounded_warmups"], 2)
        self.assertEqual(result["observed_read_below_half_of_prefix"], 0)

    def test_projection_uses_fitting_tail_and_l1_cap(self):
        step = Step(request(1, 5000), 100, 100, 5000, 10_000, False, False, l2_demand=400)
        args = profile(), step, 2000, 500, False
        self.assertEqual(fitted_projection(Settings(4000, 0.5, 200), *args), (800, 200, 500))
        self.assertEqual(fitted_projection(Settings(4000, 0.5, 200, 250), *args), (400, 200, 100))

    def test_reads_match_across_tool_interfaces(self):
        def tool(name, args):
            return {"Type": "tool_call", "Data": {"Name": name, "Arguments": json.dumps(args)}}

        native = tool_actions(tool("claude_code", {"tool": "Read", "input": {"file_path": "./a.go"}}))
        self.assertEqual(native, tool_actions(tool("Bash", {"command": "cat a.go"})))
        self.assertEqual(tool_actions(tool("Bash", {"command": "cat a.go | wc -l"}))[0][0], "repeat")
        batch = tool_actions(tool("read", {"files": [{"path": "a.go"}, {"path": "b.go"}, {"path": "./a.go"}]}))
        self.assertEqual(batch, native + tool_actions(tool("read", {"path": "b.go"})))


class NativeAuditTest(unittest.TestCase):
    def test_finds_capped_block_on_resume_and_check_fresh_rotation(self):
        def at(second):
            return f"2026-11-23T12:10:{second:02}Z"

        def event(seq, second, data):
            return {"data": {"Item": {"Sequence": seq, "RecordedAt": at(second),
                                      "Kind": "host_record", "Data": data}}}

        def bind(seq, second, identifier):
            return event(seq, second, {"Context": {"Kind": "provider_conversation_binding",
                         "ConversationBinding": {"ID": identifier, "Boundary": "rec:test/1"}}})

        def launch(seq, second):
            return event(seq, second, {"Activity": {"Kind": "claude_startup", "StartedAt": at(second)}})

        def api(identifier, second, keep, messages):
            return {"type": "api-request", "id": identifier, "timestamp": at(second), "querySource": "sdk",
                    "shapeHash": "shape", "keep": keep, "tail": messages}

        def blob(identifier, block):
            return {"type": "api-request-blob", "hash": identifier,
                    "message": {"role": "assistant", "content": [block]}}

        def write(path, rows):
            path.write_text("".join(json.dumps(row) + "\n" for row in rows))

        signed = {"type": "thinking", "thinking": "stored", "signature": "trial"}
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            session = root / "main.session.jsonl"
            write(session, [bind(1, 0, "first"), launch(2, 0), launch(3, 3), bind(4, 6, "second"), launch(5, 6)])
            write(root / "first.jsonl", [
                api("first", 1, 0, []),
                {"type": "assistant", "requestRef": "first",
                 "message": {"content": [signed], "stop_reason": "max_tokens"}},
                blob("replay", signed), api("resumed", 4, 0, ["replay"]),
            ])
            write(root / "second.jsonl", [api("new", 7, 0, [])])
            result = audit(session, root, 0)["since"]
        self.assertEqual(result["capped_native_requests"], 1)
        self.assertEqual(result["capped_replayed_on_resume_thinking"], 1)
        self.assertEqual(result["same_window_launches_with_exact_signed_blocks"], 1)
        self.assertEqual(result["rotation_first_requests_with_signatures"], 0)
        self.assertEqual(result["missing_request_blobs"], 0)


class PolicyReplayTest(unittest.TestCase):
    at = "2026-11-24T00:00:01Z"

    def write_session(self, path, provider, model, role="", recorded_role=""):
        def row(seq, kind, data):
            return {"data": {"Item": {"Sequence": seq, "RecordedAt": self.at, "Kind": kind, "Data": data}}}

        rows = [
            row(1, "host_record", {"Kind": "configuration", "Configuration": {"Layer": role}}),
            row(2, "host_record", {"Kind": "runtime_applied",
                                   "Selection": {"Provider": provider, "Model": model, "Revision": 1}}),
            row(3, "turn", {"ID": "turn", "Type": "regular", "RuntimeRevision": 1,
                            "ContextAdmission": {"EstimatedInputTokens": 8000, "BudgetTokens": 94_880,
                                                 "WindowTokens": 100_000, "WindowSource": "catalog"}}),
        ]
        if provider == "claude":
            rows.append(row(4, "host_record", {"Activity": {"Kind": "claude_stream", "Provider": provider,
                "Model": model, "ID": "turn/stream/1", "Reason": json.dumps({"event": "message_start"}),
                "RequestUsage": {"Input": 8000, "Read": 1000, "Write": 0}}}))
        else:
            rows.append(row(4, "model_response", {"TurnID": "turn", "Response": {
                "Model": model, "Usage": {"InputTokens": 8000, "CachedInputTokens": 1000}}}))
        if recorded_role:
            rows.append(row(5, "host_record", {"Context": {"Kind": "context_policy", "Policy": {"Role": recorded_role}}}))
        path.write_text("".join(json.dumps(row) + "\n" for row in rows), encoding="utf-8")

    def test_cli_replays_resolved_policy_for_each_provider(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            config = root / "runtime.json"
            config.write_text(json.dumps({"Context": {
                "WindowThresholdTokens": 15_000, "L2TargetTokens": 1000,
                "Routes": {
                    "claude": {"WindowThresholdTokens": 5000, "PostRebuildRatio": 0.3, "L2TargetTokens": 500},
                    "opencode-go": {"WindowThresholdTokens": 6000, "PostRebuildRatio": 0.6, "InputBudget": 5000},
                    "openai-codex": {"PostRebuildRatio": 0.8},
                },
            }}), encoding="utf-8")
            for name, provider, model in (
                ("child", "claude", "opus"), ("main", "openai-codex", "gpt"),
                ("deepseek", "opencode-go", "deepseek-v4.1-flash"), ("glm", "opencode-go", "glm"),
            ):
                self.write_session(root / f"{name}.session.jsonl", provider, model)
            output = StringIO()
            with patch.object(sys, "argv", ["context_replay.py", str(root), "--config", str(config), "--since", self.at]), redirect_stdout(output):
                main()
            report = json.loads(output.getvalue())

        for route, provider, ratio, rebuilds, expected_input in (
            ("claude", "claude", 0.3, 1, 1500),
            ("codex", "openai-codex", 0.8, 0, 8000),
            ("deepseek", "opencode-go", 0.6, 1, 3000),
            ("opencode-go", "opencode-go", 0.6, 1, 3000),
        ):
            with self.subTest(route=route):
                candidate = report[route]["candidate"]
                self.assertEqual(candidate["provider"], provider)
                self.assertEqual(candidate["policy"]["PostRebuildRatio"], ratio)
                self.assertEqual(candidate["holdout"]["rebuilds"], rebuilds)
                self.assertEqual(candidate["holdout"]["predicted"]["input"], expected_input)

    def test_cli_replays_each_role_on_the_same_provider(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            config, request_csv = root / "runtime.json", root / "calls.csv"
            config.write_text(json.dumps({"Context": {"L2TargetTokens": 1000, "Routes": {
                "opencode-go": {"WindowThresholdTokens": 6000, "PostRebuildRatio": 0.6, "InputBudget": 5000,
                                "Roles": {"lead": {"WindowThresholdTokens": 4000, "InputBudget": 3000}}},
            }}}), encoding="utf-8")
            self.write_session(root / "lead.session.jsonl", "opencode-go", "deepseek", role="lead")
            self.write_session(root / "worker.session.jsonl", "opencode-go", "deepseek", recorded_role="worker")
            output = StringIO()
            args = ["context_replay.py", str(root), "--config", str(config), "--since", self.at, "--request-csv", str(request_csv)]
            with patch.object(sys, "argv", args), redirect_stdout(output):
                main()
            with request_csv.open(newline="") as stream:
                inputs = {row["role"]: int(row["input"]) for row in csv.DictReader(stream)}
        candidate = json.loads(output.getvalue())["deepseek"]["candidate"]
        self.assertEqual(inputs, {"lead": 1800, "worker": 3000})
        self.assertEqual(candidate["role_policies"]["lead"]["PostRebuildRatio"], 0.6)
        self.assertEqual(candidate["holdout"]["predicted"]["input"], 4800)

    def test_candidate_rejects_invalid_effective_limits(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "runtime.json"
            for override, error in (
                ({"PostRebuildRatio": 0}, "PostRebuildRatio"),
                ({"PostRebuildRatio": 1}, "PostRebuildRatio"),
                ({"WindowThresholdTokens": 0}, "WindowThresholdTokens"),
                ({"WindowThresholdTokens": 9999}, "L2TargetTokens"),
            ):
                path.write_text(json.dumps({"Context": {"Routes": {"claude": override}}}), encoding="utf-8")
                with self.subTest(override=override), self.assertRaisesRegex(ValueError, error):
                    read_context_policies(path)


if __name__ == "__main__":
    unittest.main()

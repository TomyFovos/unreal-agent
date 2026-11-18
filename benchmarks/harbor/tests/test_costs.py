import json
import subprocess
import sys
import unittest
from pathlib import Path
from tempfile import TemporaryDirectory
from unittest.mock import patch

from test_product import response, session_file, usage

from harness_harbor.costs import recalculate
from harness_harbor.pricing import estimate, load_prices
from harness_harbor.product import aggregate, usage_rows


def activity(kind, turn, model=None, counts=None, **fields):
    return "host_record", {
        "Kind": "activity",
        "Activity": {
            "Kind": kind,
            "ID": turn,
            "Provider": "claude-api",
            **({"Model": model} if model else {}),
            **({"Usage": counts} if counts is not None else {}),
            **fields,
        },
    }


def items(*values):
    return [{"Kind": kind, "Data": data} for kind, data in values]


IDENTITY = {"Provider": {"provider": "claude-api", "model": {"id": "opus"}}}
PRICES = {
    "anthropic/exact-opus": {
        "input_cost_per_token": 4e-6,
        "output_cost_per_token": 20e-6,
        "cache_read_input_token_cost": 0.2e-6,
        "cache_creation_input_token_cost": 5e-6,
    },
    "openai/exact-gpt": {"input_cost_per_token": 1e-6, "output_cost_per_token": 10e-6},
}


class CostTests(unittest.TestCase):
    def test_execution_total_replaces_all_same_turn_fragments(self):
        rows = usage_rows(
            items(
                response("t1", usage(111, 222)),
                ("turn", {"ID": "tool-continuation", "ToolContinuation": "t1"}),
                response("tool-continuation", usage(333, 444)),
                activity("provider_usage", "t1", "<synthetic>", usage(0, 0)),
                activity("provider_usage", "t1", "exact-opus", usage(115, 2, 100, 5)),
                activity("claude_usage", "t1", counts=usage(115, 128000, 100, 5)),
                activity("provider_usage", "t2", "exact-opus", usage(30, 1)),
                activity("claude_usage", "t2", counts=usage(30, 8192)),
            ),
            IDENTITY,
        )
        self.assertEqual(
            rows,
            [
                (
                    "claude-api",
                    "exact-opus",
                    {**usage(115, 128000, 100, 5), "Unknown": []},
                ),
                ("claude-api", "exact-opus", {**usage(30, 8192), "Unknown": []}),
            ],
        )
        connections, totals = aggregate(rows, prices=PRICES)
        self.assertEqual(totals["output_tokens"], 136192)
        self.assertIsNotNone(totals["cost_usd"])
        self.assertEqual(set(connections["claude-api"]["models"]), {"exact-opus"})

    def test_unfinished_execution_still_counts_its_tool_receipts(self):
        rows = usage_rows(
            items(
                ("turn", {"ID": "unfinished"}),
                response("unfinished", usage(10, 20)),
                ("turn", {"ID": "continuation", "ToolContinuation": "unfinished"}),
                response("continuation", usage(30, 40)),
                activity("claude_usage", "completed", "exact-opus", usage(1, 2)),
            ),
            IDENTITY,
        )
        _, totals = aggregate(rows, prices=PRICES)
        self.assertEqual(totals["input_tokens"], 41)
        self.assertEqual(totals["output_tokens"], 62)

    def test_execution_without_model_fragments_keeps_exact_model(self):
        rows = usage_rows(
            items(activity("claude_usage", "t", "exact-opus", usage(10, 128000))),
            IDENTITY,
        )
        self.assertEqual(rows[0][:2], ("claude-api", "exact-opus"))
        self.assertEqual(rows[0][2]["OutputTokens"], 128000)

    def test_native_models_share_one_execution_total(self):
        rows = usage_rows(
            items(
                response("t", usage(999, 999)),
                activity("provider_usage", "t", "exact-opus", usage(100, 1, 20, 30)),
                activity("provider_usage", "t", "native-model", usage(10, 1, 3)),
                activity(
                    "claude_stream",
                    "t/stream/1",
                    "exact-opus",
                    Reason=json.dumps({"event": "message_start"}),
                ),
                activity(
                    "claude_stream",
                    "t/stream/2",
                    "native-model",
                    Reason=json.dumps(
                        {
                            "event": "message_stop",
                            "parent_tool_use_id": "agent-call",
                            "output_tokens": 7,
                        }
                    ),
                ),
                activity("claude_usage", "t", counts=usage(110, 27, 23, 30)),
            ),
            IDENTITY,
        )
        self.assertEqual(
            rows,
            [
                ("claude-api", "exact-opus", {**usage(100, 20, 20, 30), "Unknown": []}),
                ("claude-api", "native-model", usage(10, 7, 3)),
            ],
        )
        _, totals = aggregate(rows, prices=PRICES)
        self.assertEqual(totals["input_tokens"], 110)
        self.assertEqual(totals["output_tokens"], 27)
        self.assertIsNone(totals["cost_usd"])

    def test_synthetic_models_never_poison_costs(self):
        connections, totals = aggregate(
            [
                ("claude-api", "<synthetic>", usage(0, 0)),
                ("claude-api", "exact-opus", usage(10, 20)),
            ],
            prices=PRICES,
        )
        self.assertEqual(set(connections["claude-api"]["models"]), {"exact-opus"})
        self.assertAlmostEqual(totals["cost_usd"], 0.00044)

    def test_price_loading_and_calculation_make_no_network_calls(self):
        with patch("socket.socket.connect", side_effect=AssertionError("network")):
            prices = load_prices()
            self.assertIn("gpt-4o", prices)
            self.assertIsNotNone(estimate("openai", "gpt-4o", usage(100, 10)))
        unknown = {**usage(10, 20), "Unknown": ["output"]}
        self.assertIsNone(estimate("claude-api", "exact-opus", unknown, prices=PRICES))

    def test_job_sessions_include_children_and_grandchildren_once(self):
        with TemporaryDirectory() as temporary:
            job = Path(temporary)
            directory = job / "task__attempt/agent/sessions"
            directory.mkdir(parents=True)
            session_file(
                directory / "main.session.jsonl",
                "claude-api",
                "opus",
                [
                    response("t", usage(999, 999)),
                    activity("provider_usage", "t", "exact-opus", usage(10, 2)),
                    activity("claude_usage", "t", counts=usage(10, 128000)),
                ],
                ["child"],
            )
            session_file(
                directory / "child.session.jsonl",
                "openai",
                "exact-gpt",
                [response("t", usage(20, 3))],
                ["grandchild"],
            )
            session_file(
                directory / "grandchild.session.jsonl",
                "openai",
                "exact-gpt",
                [response("t", usage(30, 4))],
            )
            before = {p: p.read_bytes() for p in directory.iterdir()}
            with (
                patch.dict(sys.modules, {"litellm": None}),
                patch("socket.socket.connect", side_effect=AssertionError("network")),
            ):
                offline = recalculate(job)
                self.assertEqual(offline["output_tokens"], 128007)
            report = recalculate(job, prices=PRICES)
            trial = report["trials"][0]
            self.assertEqual(report["output_tokens"], 128007)
            self.assertEqual(report["input_tokens"], 60)
            self.assertEqual(trial["task"], "task")
            self.assertEqual(trial["sessions"]["main"]["role"], "main")
            self.assertEqual(trial["sessions"]["grandchild"]["role"], "child")
            self.assertEqual(
                trial["usage_by_connection"]["openai"]["models"]["exact-gpt"][
                    "output_tokens"
                ],
                7,
            )
            prices = job / "prices.json"
            prices.write_text(json.dumps(PRICES))
            command = Path(__file__).parents[1] / "recalculate_costs.py"
            result = subprocess.run(
                [sys.executable, command, str(job), "--prices", str(prices)],
                check=True,
                capture_output=True,
                text=True,
            )
            saved = json.loads(result.stdout)
            self.assertEqual(saved["price_table"], str(prices))
            self.assertEqual(saved["trials"], report["trials"])
            self.assertEqual(before, {p: p.read_bytes() for p in directory.iterdir()})
            # Offline recalculation must retain archived descendants exactly
            # once, including a transaction between index write and record move.
            archive = directory.with_name("sessions-archive")
            for child, parent, kind in (
                ("child", "main", "index"),
                ("grandchild", "child", "pending"),
            ):
                target = archive / parent / child / f"{child}.session.jsonl"
                target.parent.mkdir(parents=True)
                (directory / target.name).rename(target)
                index = archive / kind / f"{child}.json"
                index.parent.mkdir(parents=True, exist_ok=True)
                index.write_text(json.dumps({"child_id": child, "parent_id": parent}))
            self.assertEqual(recalculate(job, prices=PRICES), report)

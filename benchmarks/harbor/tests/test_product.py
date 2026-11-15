import asyncio
import hashlib
import importlib.util
import io
import json
import os
import subprocess
import sys
import unittest
from pathlib import Path
from tempfile import TemporaryDirectory
from types import SimpleNamespace
from unittest.mock import patch

from harbor.environments.base import ExecResult
from harbor.models.agent.context import AgentContext
from harbor.models.trajectories import Agent

from harness_harbor.agent import UnrealAgent
from harness_harbor.product import aggregate, export_product, usage_rows
from harness_harbor.security import Redactor, read_key, scan


def bundle(path):
    files = {"unreal-agent": b"product-host", "claude-install.py": b"installer"}
    for name, data in {**files, "unreal-agent-runner": b"runner"}.items():
        (path / name).write_bytes(data)
    (path / "manifest.json").write_text(
        json.dumps(
            {
                "revision": "b" * 40,
                "goos": "linux",
                "goarch": "amd64",
                "sha256": hashlib.sha256(b"runner").hexdigest(),
                "files": {
                    name: hashlib.sha256(data).hexdigest()
                    for name, data in files.items()
                },
            }
        )
    )


def usage(inputs, outputs, cached=0, writes=0, reasoning=0):
    return {
        "InputTokens": inputs,
        "OutputTokens": outputs,
        "CachedInputTokens": cached,
        "CacheWriteInputTokens": writes,
        "ReasoningTokens": reasoning,
    }


def session_file(path, provider, model, items, children=()):
    identity = {
        "Version": 1,
        "Provider": {"provider": provider, "model": {"id": model}},
    }
    envelopes = []
    for sequence, (kind, data) in enumerate(
        [
            (
                "host_record",
                {"Version": 1, "Kind": "configuration", "Configuration": identity},
            ),
            *items,
        ],
        1,
    ):
        envelopes.append(
            {
                "type": "item",
                "data": {
                    "Item": {
                        "Sequence": sequence,
                        "RecordedAt": "2026-11-13T00:00:00Z",
                        "Kind": kind,
                        "Data": data,
                    }
                },
            }
        )
    for child in children:
        envelopes.append(
            {
                "type": "operation",
                "data": {
                    "Operation": {
                        "ID": "op-" + child,
                        "State": {
                            "Plan": {
                                "Type": "subagent",
                                "Data": {
                                    "Action": "start",
                                    "ChildID": child,
                                    "ParentID": path.stem,
                                    "Template": "lead",
                                },
                            }
                        },
                    }
                },
            }
        )
    path.write_text("".join(json.dumps(e) + "\n" for e in envelopes))


def response(turn, counts, text="done"):
    return "model_response", {
        "TurnID": turn,
        "Response": {
            "ID": turn,
            "Stop": "complete",
            "Usage": counts,
            "Output": [{"Type": "message", "Data": {"Text": text}}],
        },
    }


class CapturingEnvironment:
    default_user = None

    def __init__(self):
        self.commands, self.uploads = [], {}

    async def upload_file(self, source, destination):
        self.uploads[destination] = (source.read_bytes(), source.stat().st_mode & 0o777)

    async def exec(self, command, **kwargs):
        self.commands.append((command, kwargs))
        return ExecResult(return_code=0)


class ProductTests(unittest.TestCase):
    def setUp(self):
        # Price-only library imports must stay offline in tests too.
        environment = patch.dict(os.environ, {"LITELLM_LOCAL_MODEL_COST_MAP": "True"})
        environment.start()
        self.addCleanup(environment.stop)

    def test_default_and_three_operator_configurations(self):
        with TemporaryDirectory() as temporary:
            root = Path(temporary)
            bundle(root)
            for mode in ("claude-gpt", "gpt", "claude"):
                agent = UnrealAgent(
                    bundle=temporary,
                    logs_dir=root,
                    configuration=mode,
                    main_effort="max",
                    child_model="anthropic/sonnet"
                    if mode == "claude"
                    else "openai/gpt-6-astra",
                    child_effort="high",
                )
                config = agent._runtime_config(
                    {
                        "openai": "/private/openai.key",
                        "claude-api": "/private/claude.key",
                    }
                )
                self.assertEqual(
                    config["Runtime"]["Provider"]["provider"],
                    "openai" if mode == "gpt" else "claude-api",
                )
                self.assertEqual(config["Runtime"]["ReasoningEffort"], "max")
                self.assertFalse(config["Launcher"]["DiscoverProviders"])
                if mode == "claude":
                    self.assertTrue(config["DisableSubagents"])
                    self.assertNotIn("SubagentDefaults", config)
                    self.assertEqual(
                        config["Runtime"]["Claude"]["AgentModel"], "sonnet"
                    )
                    self.assertEqual(config["Runtime"]["Claude"]["AgentEffort"], "high")
                else:
                    self.assertEqual(
                        config["SubagentDefaults"],
                        {
                            "Provider": "openai",
                            "Model": "gpt-6-astra",
                            "Effort": "high",
                        },
                    )
            self.assertEqual(
                UnrealAgent(bundle=temporary, logs_dir=root)._configuration,
                "claude-gpt",
            )
            for kwargs in (
                {"configuration": "unknown"},
                {"child_effort": "extreme"},
                {"configuration": "gpt", "main_model": "anthropic/opus"},
                {"child_model": "anthropic/sonnet"},
            ):
                with self.assertRaises(ValueError):
                    UnrealAgent(bundle=temporary, logs_dir=root, **kwargs)

    def test_key_upload_permissions_and_commands_contain_no_values(self):
        with TemporaryDirectory() as temporary:
            root = Path(temporary)
            bundle(root)
            key = root / "api.key"
            key.write_text("fake-openai-private-key")
            key.chmod(0o600)
            agent = UnrealAgent(
                bundle=temporary,
                logs_dir=root,
                configuration="gpt",
                openai_key_file=str(key),
            )
            environment = CapturingEnvironment()
            asyncio.run(agent.run("Fix the issue.", environment, AgentContext()))
            remote_key = str(agent._state / "openai.key")
            self.assertEqual(
                environment.uploads[remote_key], (b"fake-openai-private-key", 0o600)
            )
            commands = str(environment.commands)
            self.assertNotIn("fake-openai-private-key", commands)
            self.assertIn("chmod 600 " + remote_key, commands)
            launch = json.loads(
                environment.uploads[str(agent._state / "launch.json")][0]
            )
            self.assertEqual(launch["keys"], {"openai": remote_key})
            self.assertNotIn("fake-openai-private-key", json.dumps(launch))
            key.chmod(0o644)
            with self.assertRaisesRegex(ValueError, "0600"):
                agent._keys()

    def test_install_uses_verified_snapshot_and_pinned_route_installer(self):
        with TemporaryDirectory() as temporary:
            root = Path(temporary)
            bundle(root)
            for mode in ("gpt", "claude-gpt", "claude"):
                agent = UnrealAgent(bundle=temporary, logs_dir=root, configuration=mode)
                environment = CapturingEnvironment()
                with (
                    patch.object(agent, "ensure_curl"),
                    patch.object(agent, "ensure_system_dependencies"),
                ):
                    asyncio.run(agent.install(environment))
                commands = str(environment.commands)
                self.assertIn(
                    "chmod 755 /installed-agent/unreal-agent/unreal-agent", commands
                )
                self.assertEqual(
                    "claude-install.py --destination" in commands, mode != "gpt"
                )
                self.assertIn("chmod 700 " + str(agent._state), commands)
            (root / "unreal-agent").write_bytes(b"changed")
            with self.assertRaisesRegex(ValueError, "checksum"):
                UnrealAgent(bundle=temporary, logs_dir=root)

    def test_two_connections_native_usage_and_recursive_children_count_once(self):
        with TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / "sessions").mkdir()
            parent = [
                response("t", usage(999, 999)),
                (
                    "host_record",
                    {
                        "Version": 1,
                        "Kind": "activity",
                        "Activity": {
                            "Kind": "provider_usage",
                            "ID": "t",
                            "Provider": "claude-api",
                            "Model": "unknown-opus",
                            "Usage": usage(100, 10, 20, 30),
                        },
                    },
                ),
                (
                    "host_record",
                    {
                        "Version": 1,
                        "Kind": "activity",
                        "Activity": {
                            "Kind": "provider_usage",
                            "ID": "t",
                            "Provider": "claude-api",
                            "Model": "unknown-haiku",
                            "Usage": usage(50, 5),
                        },
                    },
                ),
            ]
            session_file(
                root / "sessions/main.session.jsonl",
                "claude-api",
                "opus",
                parent,
                ["child"],
            )
            session_file(
                root / "sessions/child.session.jsonl",
                "openai",
                "unknown-gpt",
                [response("c", usage(30, 3, 4, 0, 1))],
                ["grandchild"],
            )
            session_file(
                root / "sessions/grandchild.session.jsonl",
                "openai",
                "unknown-gpt",
                [response("g", usage(20, 2, 2, 0, 1))],
            )
            trajectory = export_product(
                root,
                Agent(name="unreal", version="test", model_name="anthropic/opus"),
                "main",
            )
            self.assertEqual(trajectory.final_metrics.total_prompt_tokens, 200)
            self.assertEqual(trajectory.final_metrics.total_completion_tokens, 20)
            self.assertEqual(trajectory.final_metrics.total_cached_tokens, 26)
            self.assertIsNone(trajectory.final_metrics.total_cost_usd)
            by_connection = trajectory.extra["usage_by_connection"]
            self.assertEqual(by_connection["openai"]["input_tokens"], 50)
            self.assertEqual(by_connection["claude-api"]["input_tokens"], 150)
            self.assertEqual(by_connection["claude-api"]["cache_write_tokens"], 30)
            self.assertEqual(by_connection["openai"]["reasoning_tokens"], 2)
            ref = (
                trajectory.steps[-1].observation.results[-1].subagent_trajectory_ref[0]
            )
            self.assertTrue((root / ref.trajectory_path).exists())
            child = json.loads((root / ref.trajectory_path).read_text())
            self.assertTrue(
                (root / child["extra"]["children"][0]["trajectory_path"]).exists()
            )

    def test_usage_without_authoritative_messages_and_unknown_reasoning(self):
        items = [
            {"Kind": kind, "Data": data}
            for kind, data in [
                response("a", {**usage(10, 2), "Unknown": ["reasoning"]}),
                response("b", usage(5, 3, reasoning=1)),
            ]
        ]
        rows = usage_rows(
            items, {"Provider": {"provider": "openai", "model": {"id": "unknown"}}}
        )
        breakdown, totals = aggregate(rows)
        self.assertEqual(totals["input_tokens"], 15)
        self.assertIsNone(totals["reasoning_tokens"])
        self.assertIsNone(breakdown["openai"]["models"]["unknown"]["reasoning_tokens"])
        _, absent = aggregate([])
        self.assertIsNone(absent["input_tokens"])
        self.assertEqual(breakdown["openai"]["unknown_usage_fields"], ["reasoning"])

    def test_exact_price_map_only_and_cache_subsets(self):
        import litellm

        with (
            patch.dict(litellm.model_cost, {"exact-test": {}}, clear=True),
            patch.object(litellm, "cost_per_token", return_value=(0.2, 0.3)) as price,
        ):
            _, totals = aggregate([("openai", "exact-test", usage(100, 10, 20, 30, 4))])
            self.assertEqual(totals["cost_usd"], 0.5)
            self.assertEqual(price.call_args.kwargs["prompt_tokens"], 100)
            self.assertEqual(price.call_args.kwargs["cache_creation_input_tokens"], 30)
            price.reset_mock()
            _, unknown = aggregate([("openai", "approximate-test", usage(100, 10))])
            self.assertIsNone(unknown["cost_usd"])
            price.assert_not_called()

    def test_offline_prices_charge_cache_subsets_once(self):
        prices = {
            "exact-test": {
                "input_cost_per_token": 0.01,
                "output_cost_per_token": 0.02,
                "cache_read_input_token_cost": 0.001,
                "cache_creation_input_token_cost": 0.015,
            }
        }
        _, totals = aggregate(
            [("openai", "exact-test", usage(100, 10, 20, 30, 4))], prices=prices
        )
        # Input includes 20 cache reads and 30 writes; charge each token once.
        self.assertAlmostEqual(totals["cost_usd"], 0.5 + 0.02 + 0.45 + 0.2)
        _, unknown = aggregate(
            [("openai", "approximate-test", usage(100, 10))], prices=prices
        )
        self.assertIsNone(unknown["cost_usd"])

    def test_no_secret_in_exports_context_or_error(self):
        with TemporaryDirectory() as temporary:
            root = Path(temporary)
            bundle(root)
            agent = UnrealAgent(bundle=temporary, logs_dir=root, configuration="gpt")
            agent._secrets = ["fake-selected-secret"]
            (root / "sessions").mkdir()
            session_file(
                root / "sessions" / f"{agent._runner_session}.session.jsonl",
                "openai",
                "unknown-model",
                [
                    response(
                        "t",
                        usage(10, 3),
                        "fake-selected-secret Authorization: Bearer oauth-private",
                    )
                ],
            )
            (root / "host.stderr").write_text(
                "fake-selected-secret sk-ant-test-privatevalue"
            )
            context = AgentContext()
            agent.populate_context_post_run(context)
            output = (
                json.dumps(context.model_dump())
                + "".join(p.read_text() for p in root.rglob("*.json*"))
                + (root / "host.stderr").read_text()
            )
            for value in (
                "fake-selected-secret",
                "oauth-private",
                "sk-ant-test-privatevalue",
            ):
                self.assertNotIn(value, output)
            self.assertEqual(context.n_input_tokens, 10)
            self.assertEqual(
                context.metadata["usage_by_connection"]["openai"]["input_tokens"], 10
            )
            missing = root / "fake-selected-secret.key"
            with self.assertRaises(ValueError) as error:
                read_key(str(missing))
            self.assertNotIn("fake-selected-secret", str(error.exception))

    def test_redaction_covers_known_encodings_and_private_replay(self):
        secret = 'fake-key/with"quotes'
        redactor = Redactor([secret])
        value = {
            "Raw": "opaque-provider-replay",
            "AuthMethod": "oauth",
            "signature": "private-signature",
            "text": secret,
            "Arguments": json.dumps({"value": secret}),
        }
        value["token"] = "synthetic-token-value"
        value["secret"] = "synthetic-secret-value"
        value["diagnostic"] = (
            "GITHUB_TOKEN=synthetic-github-value token=synthetic-auth-value"
        )
        encoded = json.dumps(redactor.clean(value))
        for sensitive in (
            "synthetic-token-value",
            "synthetic-secret-value",
            "synthetic-github-value",
            "synthetic-auth-value",
        ):
            self.assertNotIn(sensitive, encoded)
        for private in (
            "opaque-provider-replay",
            "private-signature",
            "oauth",
            "fake-key",
        ):
            self.assertNotIn(private, encoded)
        with TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / "failure.log").write_text(secret)
            self.assertEqual(scan([root], [secret]), [str(root / "failure.log")])

    def test_attachment_exports_sanitized_failure_with_private_key_files(self):
        with TemporaryDirectory() as temporary:
            root = Path(temporary)
            state, logs = root / "private", root / "public"
            state.mkdir(mode=0o700)
            key = state / "openai.key"
            key.write_text("fake-attachment-secret")
            key.chmod(0o600)
            fake = root / "host"
            fixture = root / "fixture.session.jsonl"
            session_file(
                fixture,
                "openai",
                "unknown",
                [response("t", usage(2, 1), "fake-attachment-secret")],
            )
            fake.write_text(
                "#!/usr/bin/env python3\nimport os,sys,pathlib\n"
                "assert not os.environ.get('OPENAI_API_KEY')\n"
                "p=pathlib.Path(sys.argv[sys.argv.index('--session-directory')+1]);p.mkdir()\n"
                + "(p/'main.session.jsonl').write_bytes(pathlib.Path("
                + repr(str(fixture))
                + ").read_bytes())\n"
                + "print('fake-attachment-secret',file=sys.stderr)\nsys.exit(3)\n"
            )
            fake.chmod(0o755)
            request = {
                "state": str(state),
                "logs": str(logs),
                "keys": {"openai": str(key)},
                "binary": str(fake),
                "bin": str(root),
                "session": "main",
                "environment": {},
            }
            config = root / "launch.json"
            config.write_text(json.dumps(request))
            launch = (
                Path(__file__).resolve().parents[1] / "src/harness_harbor/launch.py"
            )
            result = subprocess.run(
                [sys.executable, str(launch), str(config)],
                env={**os.environ, "OPENAI_API_KEY": "fake-ambient-key"},
                capture_output=True,
                text=True,
            )
            self.assertEqual(result.returncode, 3, result.stderr)
            self.assertEqual((logs / "host.stderr").read_text(), "[redacted]\n")
            self.assertFalse(
                scan([logs], ["fake-attachment-secret", "fake-ambient-key"])
            )
            self.assertFalse((logs / "openai.key").exists())

    def test_smoke_peer_echoes_the_input_and_finishes_the_claude_protocol(self):
        source = Path(__file__).with_name("fake_claude.py")
        spec = importlib.util.spec_from_file_location("smoke_peer", source)
        peer = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(peer)
        frames = []
        with TemporaryDirectory() as temporary:
            incoming = [
                {"type": "control_request", "request_id": "init", "request": {}},
                {"type": "user", "message": {"role": "user", "content": "Task"}},
                None,
            ]
            with (
                patch.object(peer, "receive", side_effect=incoming),
                patch.object(peer, "send", side_effect=frames.append),
                patch.object(peer, "ask", return_value={"behavior": "allow"}),
                patch.object(peer.Path, "home", return_value=Path(temporary)),
                patch.object(
                    peer.subprocess, "run", return_value=SimpleNamespace(stdout="done")
                ) as shell,
                patch.object(
                    peer.sys,
                    "argv",
                    ["claude", "--append-system-prompt", "No GPT children available"],
                ),
            ):
                peer.main()
            shell.assert_called_once()
        self.assertTrue(any(f.get("isReplay") for f in frames))
        self.assertEqual(frames[-1]["type"], "result")
        self.assertEqual(frames[-1]["subtype"], "success")

    def test_route_installer_verifies_integrity_before_extracting(self):
        path = (
            Path(__file__).resolve().parents[3]
            / "harness/llm/clients/claudeagent/install.py"
        )
        spec = importlib.util.spec_from_file_location("claude_runtime_installer", path)
        installer = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(installer)
        with (
            patch.object(
                installer.urllib.request,
                "urlopen",
                return_value=io.BytesIO(b"bad archive"),
            ),
            self.assertRaisesRegex(ValueError, "integrity"),
        ):
            installer.verified_download("https://test.invalid", "sha256", bytes(32))
        self.assertEqual(installer.CLAUDE_VERSION, "2.1.322")
        self.assertEqual(installer.NODE_VERSION, "22.16.0")
        self.assertTrue(
            all(
                len(__import__("base64").b64decode(v)) == 64
                for v in installer.CLAUDE_INTEGRITY.values()
            )
        )


if __name__ == "__main__":
    unittest.main()

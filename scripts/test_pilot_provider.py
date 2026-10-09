import contextlib
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).parent))
import pilot_provider as pilot
import provider_pilot_bundle as bundle


def final_view(text):
    return {"Running": True, "Operations": [], "History": {"More": False, "Items": [
        {"Kind": "model_response", "Data": {"Response": {"Stop": "complete", "Output": [
            {"Type": "message", "Data": {"Role": "assistant", "Text": text}}]}}}]}}


class ProviderPilotTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="uap-", dir="/tmp")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.home = self.root / "home"
        self.home.mkdir(mode=0o700)
        self.workspace = self.home / "workspace"
        self.workspace.mkdir()

    def test_provider_split_and_read_only_permissions(self):
        for sandbox in pilot.PROVIDERS:
            with self.subTest(sandbox=sandbox):
                cfg = pilot.configuration(sandbox, self.workspace, "fixture-model", "high", "/unused/claude", sandbox == "devlore", sandbox == "devlore")
                self.assertEqual(cfg["Runtime"]["Provider"]["provider"], pilot.PROVIDERS[sandbox])
                self.assertEqual(cfg["Runtime"]["Provider"]["max_attempts"], 1)
                self.assertFalse(cfg["Launcher"]["DiscoverProviders"])
                self.assertNotIn("Providers", cfg)
                permissions = cfg["Permissions"]
                self.assertEqual(permissions["Tools"], ["read", "grep", "glob"])
                self.assertEqual(permissions["ReadRoots"], [str(self.workspace)])
                self.assertEqual(permissions["WriteRoots"], [])
                self.assertFalse(permissions["FilesystemUnrestricted"])
                self.assertFalse(permissions["NetworkUnrestricted"])
                self.assertEqual(permissions["ProcessMode"], "")
                if sandbox == "devlore":
                    self.assertEqual(cfg["Runtime"]["ClaudeCode"]["ToolBridge"]["Mode"], "structured")
                    self.assertEqual(cfg["Runtime"]["ClaudeCode"]["managedPolicyMode"], "trust")
                    self.assertNotIn("NetworkOrigins", permissions)
                else:
                    self.assertNotIn("ClaudeCode", cfg["Runtime"])
                    self.assertEqual(permissions["NetworkOrigins"], ["https://chatgpt.com"])

    def test_trust_needs_both_explicit_conditions(self):
        for audited, trust in ((False, False), (True, False), (False, True)):
            with self.subTest(audited=audited, trust=trust), self.assertRaises(pilot.PilotError):
                pilot.configuration("devlore", self.workspace, "fixture", "high", audited=audited, trust=trust)
        with self.assertRaises(pilot.PilotError):
            pilot.configuration("asp", self.workspace, "fixture", "medium", audited=True, trust=True)

    def test_unreviewed_policy_stops_before_cli_and_config_creation(self):
        with patch.dict(os.environ, {"HOME": str(self.home)}, clear=True), patch.object(pilot, "installed_kit") as kit, \
                patch.object(pilot, "bounded_probe") as probe, contextlib.redirect_stderr(io.StringIO()) as errors:
            self.assertEqual(pilot.main(["devlore", "--model", "fixture", "--preflight-only"]), 1)
            self.assertIn("managed_policy_audit_required", errors.getvalue())
        kit.assert_not_called()
        probe.assert_not_called()
        self.assertFalse((self.home / ".config").exists())

    def auth_fixture(self):
        path = self.home / ".codex/auth.json"
        path.parent.mkdir(mode=0o700)
        path.write_text("PRIVATE_DUMMY_NOT_JSON_MUST_NOT_READ")
        path.chmod(0o600)
        return path

    def test_codex_metadata_only_and_missing_auth(self):
        with self.assertRaises(pilot.PilotError) as failure:
            pilot.codex_preflight(self.home, {})
        self.assertEqual(failure.exception.code, "codex_login_required")
        path = self.auth_fixture()
        before = path.stat().st_mtime_ns
        with patch.object(Path, "read_bytes", side_effect=AssertionError("must not read auth")), \
                patch.object(Path, "read_text", side_effect=AssertionError("must not read auth")), contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(pilot.codex_preflight(self.home, {}), str(path))
        self.assertEqual(path.stat().st_mtime_ns, before)

    def test_codex_override_priority_private_file_and_symlink_refusal(self):
        path = self.auth_fixture()
        other = self.root / "explicit-auth"
        other.write_text("private dummy")
        other.chmod(0o600)
        with contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(pilot.codex_preflight(self.home, {"OPENAI_CODEX_AUTH_FILE": str(other), "CODEX_HOME": str(path.parent)}), str(other))
        other.chmod(0o644)
        with self.assertRaises(pilot.PilotError):
            pilot.codex_preflight(self.home, {"OPENAI_CODEX_AUTH_FILE": str(other)})
        alias = self.root / "alias"
        alias.symlink_to(path)
        with self.assertRaises(pilot.PilotError):
            pilot.codex_preflight(self.home, {"OPENAI_CODEX_AUTH_FILE": str(alias)})

    def claude_probe(self, raw=None, status=0):
        auth = raw or json.dumps({"loggedIn": True, "authMethod": "claude.ai", "apiProvider": "firstParty", "subscriptionType": "team", "email": "PRIVATE_ACCOUNT_DUMMY"}).encode()
        calls = []

        def fake(args, env, **kwargs):
            calls.append((args, env))
            if args[-1] == "--version":
                return b"2.1.285 (Claude Code)\n", 0
            if args[-1] == "--help":
                return b" ".join(flag.encode() for flag in ("--safe-mode --restricted --setting-sources --settings --tools --disallowedTools --strict-mcp-config --mcp-config --disable-slash-commands --no-chrome --no-session-persistence --model --effort --input-format --output-format --verbose --include-partial-messages --permission-prompts --allowedTools --json-schema").split()), 0
            return auth, status
        return calls, fake

    def test_claude_auth_status_isolated_no_credentials_or_account_output(self):
        calls, fake = self.claude_probe()
        with patch.object(pilot.shutil, "which", return_value="/unused/claude"), patch.object(pilot, "bounded_probe", side_effect=fake), contextlib.redirect_stdout(io.StringIO()) as output:
            result = pilot.claude_preflight("claude", self.home, {"GITHUB_TOKEN": "PRIVATE_SECRET", "ANTHROPIC_API_KEY": "PRIVATE_SECRET", "OPENAI_API_KEY": "PRIVATE_SECRET"}, True, True)
        self.assertEqual(result, "/unused/claude")
        self.assertEqual(len(calls), 3)
        auth_args, env = calls[-1]
        self.assertEqual(auth_args[-2:], ["auth", "status"])
        self.assertEqual(auth_args[auth_args.index("--tools") + 1], "")
        self.assertEqual(auth_args[auth_args.index("--mcp-config") + 1], '{"mcpServers":{}}')
        self.assertEqual(auth_args[auth_args.index("--disallowedTools") + 1], "*")
        self.assertNotIn("PRIVATE_SECRET", json.dumps(env))
        self.assertNotIn("PRIVATE_ACCOUNT_DUMMY", output.getvalue())

    def test_claude_missing_unsigned_and_invalid_status_refused(self):
        with patch.object(pilot.shutil, "which", return_value=None), self.assertRaises(pilot.PilotError) as failure:
            pilot.claude_preflight("claude", self.home, {}, True, True)
        self.assertEqual(failure.exception.code, "claude_binary_missing")
        cases = [(b'{"loggedIn":false,"private":"PRIVATE_DUMMY"}', 1, "claude_login_required"),
                 (b'{"loggedIn":"true","private":"PRIVATE_DUMMY"}', 0, "claude_status_invalid"),
                 (b'PRIVATE_DUMMY malformed', 0, "claude_status_invalid"),
                 (b'{"loggedIn":true,"authMethod":"api_key","apiProvider":"firstParty","subscriptionType":"team"}', 0, "claude_subscription_unavailable")]
        for raw, status, code in cases:
            with self.subTest(code=code):
                _, fake = self.claude_probe(raw, status)
                with patch.object(pilot.shutil, "which", return_value="/unused/claude"), patch.object(pilot, "bounded_probe", side_effect=fake), self.assertRaises(pilot.PilotError) as error:
                    pilot.claude_preflight("claude", self.home, {}, True, True)
                self.assertEqual(error.exception.code, code)
                self.assertNotIn("PRIVATE_DUMMY", str(error.exception))

    def test_claude_routing_conflict_is_not_silently_dropped(self):
        for env in ({"CLAUDE_CODE_USE_BEDROCK": "true"}, {"ANTHROPIC_BASE_URL": "PRIVATE_DUMMY"}):
            with self.subTest(env=env), patch.object(pilot, "bounded_probe") as probe, self.assertRaises(pilot.PilotError) as error:
                pilot.claude_preflight("claude", self.home, env, True, True)
            self.assertEqual(error.exception.code, "claude_subscription_mode_conflict")
            probe.assert_not_called()

    def test_preflight_only_private_config_rerun_and_existing_defaults_preserved(self):
        self.auth_fixture()
        defaults = {".config/unreal-agent/runtime.json": b"keep default runtime", ".local/state/unreal-agent/session.json": b"keep canonical history",
                    ".codex/config.toml": b"keep Codex config", ".claude/settings.json": b"keep Claude settings"}
        for name, data in defaults.items():
            path = self.home / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(data)
        with patch.dict(os.environ, {"HOME": str(self.home)}, clear=True), patch.object(pilot, "installed_kit", return_value=self.root), \
                patch.object(pilot, "ensure_host") as host, contextlib.redirect_stdout(io.StringIO()):
            args = ["asp", "--model", "fixture-model", "--preflight-only"]
            self.assertEqual(pilot.main(args), 0)
            config = self.home / ".config/unreal-agent-pilot/asp/v1/runtime.json"
            before = config.stat().st_mtime_ns
            self.assertEqual(pilot.main(args), 0)
            self.assertEqual(config.stat().st_mtime_ns, before)
            self.assertEqual(config.stat().st_mode & 0o777, 0o600)
            self.assertEqual(config.parent.stat().st_mode & 0o777, 0o700)
            host.assert_not_called()
        for name, data in defaults.items():
            self.assertEqual((self.home / name).read_bytes(), data)

    def test_changed_config_and_symlink_never_overwritten(self):
        folder = pilot.private_tree(self.home, "pilot")
        config = folder / "runtime.json"
        pilot.publish_json(config, {"keep": "existing operator settings"})
        with self.assertRaises(pilot.PilotError):
            pilot.publish_json(config, {"different": True})
        self.assertEqual(json.loads(config.read_text()), {"keep": "existing operator settings"})
        config.unlink()
        target = self.root / "protected"
        target.write_text("keep")
        config.symlink_to(target)
        with self.assertRaises(pilot.PilotError):
            pilot.publish_json(config, {})
        self.assertEqual(target.read_text(), "keep")

    def test_startup_arguments_fixed_provider_private_socket_no_default_paths(self):
        paths = {"config": self.root / "config", "state": self.root / "state", "sessions": self.root / "sessions", "socket": self.root / "socket"}
        serve = pilot.host_arguments(self.root, paths, "/existing/auth.json")
        launch = pilot.launcher_arguments(self.root, paths, "/existing/auth.json", "test-session")
        self.assertEqual(serve[1], "serve")
        self.assertIn("--session-directory", serve)
        self.assertIn("--state-directory", launch)
        self.assertIn("--codex-auth-file", launch)
        self.assertNotIn("--normal-runtime", serve)
        self.assertEqual(launch[-1], "test-session")
        self.assertEqual(pilot.host_binding(paths, "/existing/auth.json")["HostBinding"]["Normal"], False)

    def test_smoke_outcomes_do_not_publish_provider_text(self):
        self.assertEqual(pilot.smoke_outcome(final_view("expected"), "expected"), "pass")
        self.assertEqual(pilot.smoke_outcome(final_view("PRIVATE_DUMMY"), "expected"), "response_mismatch")
        self.assertEqual(pilot.smoke_outcome({"Failure": "PRIVATE_DUMMY"}, "expected"), "provider_failure")
        self.assertEqual(pilot.smoke_outcome({"Operations": [{}]}, "expected"), "unexpected_operation")
        for malformed in ([], {"History": []}, {"History": {"Items": ["PRIVATE_DUMMY"]}},
                          {"History": {"Items": [{"Kind": "model_response", "Data": {"Response": "PRIVATE_DUMMY"}}]}}):
            self.assertEqual(pilot.smoke_outcome(malformed, "expected"), "protocol_failure")

    def test_smoke_one_submit_and_durable_pointer_no_replay(self):
        expected = "Unreal asp provider smoke OK."
        calls = []
        class Fake:
            def call(self, method, payload):
                calls.append((method, payload))
                if method == "open":
                    return {"View": {"Generation": "DUMMY_GENERATION"}}
                if method == "inspect":
                    return {"View": final_view(expected)}
                return {"Receipt": {}}
        with contextlib.redirect_stdout(io.StringIO()) as output:
            session = pilot.smoke(Fake(), "asp", self.root / "pointer")
        self.assertEqual([method for method, _ in calls], ["open", "submit", "inspect"])
        self.assertEqual(json.loads((self.root / "pointer").read_text())["session"], session)
        self.assertNotIn(session, output.getvalue())
        self.assertNotIn("DUMMY_GENERATION", output.getvalue())

    def test_provider_failure_preserves_pointer_no_retry(self):
        calls = []
        class Fake:
            def call(self, method, payload):
                calls.append(method)
                return {"View": {"Generation": "DUMMY"}} if method == "open" else {"View": {"Failure": "PRIVATE_DUMMY"}}
        with self.assertRaises(pilot.PilotError) as failure:
            pilot.smoke(Fake(), "devlore", self.root / "pointer")
        self.assertEqual(calls.count("submit"), 1)
        self.assertTrue((self.root / "pointer").exists())
        self.assertEqual(failure.exception.code, "smoke_provider_failure")
        self.assertNotIn("PRIVATE_DUMMY", str(failure.exception))

    def test_bounded_status_timeout_and_output_limit(self):
        env = {"PATH": "/usr/bin:/bin", "HOME": str(self.home)}
        with self.assertRaises(pilot.PilotError) as error:
            pilot.bounded_probe([sys.executable, "-c", "print('x'*4096)"], env, limit=32)
        self.assertEqual(error.exception.code, "preflight_output_limit")
        with self.assertRaises(pilot.PilotError) as error:
            pilot.bounded_probe([sys.executable, "-c", "import time; time.sleep(5)"], env, timeout=0.1)
        self.assertEqual(error.exception.code, "preflight_timeout")

    def test_stderr_capture_requires_optin_and_remains_private_and_bounded(self):
        env = {"PATH": "/usr/bin:/bin", "HOME": str(self.home)}
        args = [sys.executable, "-c", "import sys; print('safe-status'); print('PRIVATE_DUMMY', file=sys.stderr); sys.exit(1)"]
        with contextlib.redirect_stdout(io.StringIO()) as output, contextlib.redirect_stderr(io.StringIO()) as errors:
            status, code = pilot.bounded_probe(args, env)
            self.assertEqual((status, code), (b"safe-status\n", 1))
            private, code = pilot.bounded_probe(args, env, capture_stderr=True)
            self.assertEqual(code, 1)
            self.assertIn(b"safe-status\n", private)
            self.assertIn(b"PRIVATE_DUMMY\n", private)
            for script, options, expected in (
                    ("import sys; print('PRIVATE_DUMMY'*4096, file=sys.stderr)", {"limit": 32}, "preflight_output_limit"),
                    ("import sys,time; print('PRIVATE_DUMMY', file=sys.stderr); time.sleep(5)", {"timeout": 0.1}, "preflight_timeout")):
                with self.assertRaises(pilot.PilotError) as failure:
                    pilot.bounded_probe([sys.executable, "-c", script], env, capture_stderr=True, **options)
                self.assertEqual(failure.exception.code, expected)
                self.assertNotIn("PRIVATE_DUMMY", str(failure.exception))
        self.assertEqual(output.getvalue(), "")
        self.assertEqual(errors.getvalue(), "")

    def test_provider_bundle_no_binaries_credentials_and_checksum(self):
        for sandbox in pilot.PROVIDERS:
            archive = bundle.build(self.root, sandbox)
            with tarfile.open(archive) as content:
                self.assertEqual({Path(member.name).name for member in content}, {"pilot.sh", "pilot_provider.py", "README.md", "SHA256SUMS"})
                self.assertTrue(all(member.isfile() for member in content))
                readme = content.extractfile(f"unreal-agent-{sandbox}-provider-6284e49/README.md").read().decode()
                self.assertNotIn("@SANDBOX@", readme)
            package = self.root / f"unreal-agent-{sandbox}-provider-6284e49"
            subprocess.run(["sha256sum", "--check", "--status", "SHA256SUMS"], cwd=package, check=True)
            subprocess.run(["bash", "-n", str(package / "pilot.sh")], check=True)

    def test_delivery_uses_fresh_provider_directory_and_preserves_old_files(self):
        archive = bundle.build(self.root, "devlore")
        destination = self.root / "windows/devlore"
        destination.mkdir(parents=True)
        existing = destination / "keep-existing-kit"
        existing.write_text("keep")
        prefix = "unreal-agent-devlore-provider-6284e49-"
        first = bundle.pilot_kit.copy_delivery(archive, destination, prefix=prefix)
        second = bundle.pilot_kit.copy_delivery(archive, destination, prefix=prefix)
        self.assertNotEqual(first, second)
        self.assertTrue(first.name.startswith(prefix))
        self.assertEqual(existing.read_text(), "keep")
        self.assertEqual((first / archive.name).read_bytes(), archive.read_bytes())
        with self.assertRaises(ValueError):
            bundle.pilot_kit.copy_delivery(archive, destination, prefix="../unsafe")

    def test_existing_kit_provenance_and_doctor_without_inference(self):
        directory = os.environ.get("UNREAL_PILOT_KIT_DIRECTORY")
        if not directory:
            self.skipTest("set UNREAL_PILOT_KIT_DIRECTORY to the existing verified kit")
        with contextlib.redirect_stdout(io.StringIO()) as output:
            self.assertEqual(pilot.installed_kit(self.home, directory), Path(directory).absolute())
        self.assertIn("6284e49", output.getvalue())


if __name__ == "__main__":
    unittest.main()

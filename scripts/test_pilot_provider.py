import contextlib
import hashlib
import io
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).parent))
import pilot_provider as pilot


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

    def test_retired_trust_flags_do_not_block_configuration(self):
        for audited, trust in ((False, False), (True, False), (False, True), (True, True)):
            cfg = pilot.configuration("devlore", self.workspace, "fixture", "high", audited=audited, trust=trust)
            self.assertEqual(cfg["Runtime"]["ClaudeCode"]["managedPolicyMode"], "trust")

    def auth_fixture(self):
        path = self.home / ".codex/auth.json"
        path.parent.mkdir(mode=0o700)
        path.write_text("PRIVATE_DUMMY_NOT_JSON_MUST_NOT_READ")
        path.chmod(0o600)
        return path


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
        self.assertEqual(len(calls), 1)
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
        binary = self.root / "synthetic-binary"; binary.write_bytes(b"not a credential")
        binding = pilot.host_binding(paths, "/existing/auth.json", binary)["HostBinding"]
        self.assertEqual(binding["Normal"], False)
        self.assertNotIn("Identity", binding)

    def test_host_binding_does_not_read_the_executable(self):
        paths = {"config": self.root / "config", "sessions": self.root / "sessions"}
        binding = pilot.host_binding(paths, "/existing/auth", self.root / "missing")["HostBinding"]
        self.assertEqual(binding["Configuration"], str(paths["config"]))
        self.assertEqual(binding["Sessions"], str(paths["sessions"]))
        self.assertNotIn("Identity", binding)

    def test_smoke_outcomes_do_not_publish_provider_text(self):
        self.assertEqual(pilot.smoke_outcome(final_view("expected"), "expected"), "pass")
        self.assertEqual(pilot.smoke_outcome(final_view("PRIVATE_DUMMY"), "expected"), "response_mismatch")
        self.assertEqual(pilot.smoke_outcome({"Failure": "PRIVATE_DUMMY"}, "expected"), "provider_failure")
        self.assertEqual(pilot.smoke_outcome({"Operations": [{}]}, "expected"), "unexpected_operation")
        for malformed in ([], {"History": []}, {"History": {"Items": ["PRIVATE_DUMMY"]}},
                          {"History": {"Items": [{"Kind": "model_response", "Data": {"Response": "PRIVATE_DUMMY"}}]}}):
            self.assertEqual(pilot.smoke_outcome(malformed, "expected"), "protocol_failure")


    def test_status_preserves_large_output(self):
        env = {"PATH": "/usr/bin:/bin", "HOME": str(self.home)}
        data, code = pilot.bounded_probe([sys.executable, "-c", "print('x'*(2<<20))"], env)
        self.assertEqual(code, 0)
        self.assertEqual(data, b"x"*(2<<20)+b"\n")

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
        self.assertEqual(output.getvalue(), "")
        self.assertEqual(errors.getvalue(), "")


if __name__ == "__main__":
    unittest.main()

import contextlib
import io
import json
import os
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).parent))
import asp_oauth as asp
import pilot_provider as pilot


class AspOAuthTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="asp-unit-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.home = self.root / "home"
        self.home.mkdir()
        (self.home / "workspace").mkdir()
        self.auth = self.home / ".codex" / "auth.json"
        self.auth.parent.mkdir()
        self.auth.write_text('{"OPENAI_API_KEY":"proxy-managed"}')
        self.auth.chmod(0o644)
        self.env = {"HOME": str(self.home), "PATH": os.environ["PATH"], "SBX_CRED_OPENAI_MODE": "oauth", "HTTPS_PROXY": "http://host.docker.internal:3128"}

    def test_exact_reference_and_readonly_policy(self):
        cfg = asp.configuration(self.home / "workspace", "gpt-6.1-sol", "medium")
        self.assertEqual(cfg["Runtime"]["Provider"]["auth"], {"provider": "openai-codex", "method": "docker_sandbox_oauth", "id": "docker-sandbox-oauth"})
        self.assertEqual(cfg["Permissions"]["Tools"], ["read", "grep", "glob"])
        self.assertEqual(cfg["Permissions"]["NetworkOrigins"], ["https://chatgpt.com"])
        self.assertEqual(cfg["Permissions"]["WriteRoots"], [])
        self.assertFalse(cfg["Permissions"]["NetworkUnrestricted"])
        self.assertFalse(cfg["Permissions"]["FilesystemUnrestricted"])
        self.assertFalse(cfg["Launcher"]["DiscoverProviders"])

    def test_no_auth_read_in_script_and_wrong_mode_denied(self):
        with patch.object(Path, "read_bytes", side_effect=AssertionError("script must not read auth")):
            self.assertEqual(asp.auth_path(self.home, self.env), str(self.auth))
        for mode in ("apikey", "none", "", "OAUTH"):
            with self.assertRaises(pilot.PilotError):
                asp.auth_path(self.home, dict(self.env, SBX_CRED_OPENAI_MODE=mode))
        self.auth.chmod(0o666)
        with self.assertRaises(pilot.PilotError):
            asp.auth_path(self.home, self.env)

    def test_native_preflight_argv_and_environment(self):
        calls = []
        def probe(args, env, **kwargs):
            calls.append((args, env, kwargs))
            return b"safe dummy", 0
        with patch.object(pilot, "bounded_probe", side_effect=probe), contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(asp.preflight(self.root, self.home, dict(self.env, OPENAI_API_KEY="PRIVATE_DUMMY", GITHUB_TOKEN="PRIVATE_DUMMY")), str(self.auth))
        args, env, kwargs = calls[0]
        self.assertEqual(args, [str(self.root / "unreal-agent-auth"), "check-docker-sandbox-oauth", "--codex-auth-file", str(self.auth), "--check-forward-proxy"])
        self.assertEqual(kwargs, {"capture_stderr": True})
        self.assertEqual(set(env), {"HOME", "PATH", "SBX_CRED_OPENAI_MODE", "HTTPS_PROXY"})
        self.assertNotIn("PRIVATE_DUMMY", json.dumps(env))
        with patch.object(pilot, "bounded_probe", return_value=(b"PRIVATE_DUMMY", 1)), self.assertRaises(pilot.PilotError) as error:
            asp.preflight(self.root, self.home, self.env)
        self.assertNotIn("PRIVATE_DUMMY", str(error.exception))
        with patch.object(pilot, "bounded_probe", return_value=(b"credential: docker_sandbox_proxy_unavailable\n", 1)), self.assertRaises(pilot.PilotError) as error:
            asp.preflight(self.root, self.home, self.env)
        self.assertEqual(error.exception.code, "docker_sandbox_proxy_unavailable")
        self.assertNotIn("PRIVATE_DUMMY", str(error.exception))
        with patch.object(pilot, "bounded_probe", return_value=(b"credential: docker_sandbox_proxy_unavailable PRIVATE_DUMMY", 1)), self.assertRaises(pilot.PilotError) as error:
            asp.preflight(self.root, self.home, self.env)
        self.assertEqual(error.exception.code, "docker_sandbox_oauth_unavailable")
        self.assertNotIn("PRIVATE_DUMMY", str(error.exception))


if __name__ == "__main__":
    unittest.main()

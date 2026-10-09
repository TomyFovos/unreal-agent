import contextlib
import io
import http.client
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).parent))
import asp_oauth as asp
import asp_oauth_bundle
import pilot_kit
import pilot_provider as pilot
from test_pilot_kit import source_archive


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
            calls.append((args, env))
            return b"safe dummy", 0
        with patch.object(pilot, "bounded_probe", side_effect=probe), contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(asp.preflight(self.root, self.home, dict(self.env, OPENAI_API_KEY="PRIVATE_DUMMY", GITHUB_TOKEN="PRIVATE_DUMMY")), str(self.auth))
        args, env = calls[0]
        self.assertEqual(args, [str(self.root / "unreal-agent-auth"), "check-docker-sandbox-oauth", "--codex-auth-file", str(self.auth), "--check-forward-proxy"])
        self.assertEqual(set(env), {"HOME", "PATH", "SBX_CRED_OPENAI_MODE", "HTTPS_PROXY"})
        self.assertNotIn("PRIVATE_DUMMY", json.dumps(env))
        with patch.object(pilot, "bounded_probe", return_value=(b"PRIVATE_DUMMY", 1)), self.assertRaises(pilot.PilotError) as error:
            asp.preflight(self.root, self.home, self.env)
        self.assertNotIn("PRIVATE_DUMMY", str(error.exception))
        with patch.object(pilot, "bounded_probe", return_value=(b"credential: docker_sandbox_proxy_unavailable PRIVATE_DUMMY", 1)), self.assertRaises(pilot.PilotError) as error:
            asp.preflight(self.root, self.home, self.env)
        self.assertEqual(error.exception.code, "docker_sandbox_proxy_unavailable")
        self.assertNotIn("PRIVATE_DUMMY", str(error.exception))

    def test_config_creation_rerun_and_existing_config_preserved(self):
        args = ["--setup-only", "--model", "gpt-6.1-sol", "--kit-directory", str(self.root)]
        with patch.dict(os.environ, self.env, clear=True), patch.object(asp, "validate_kit", return_value=self.root), patch.object(asp, "preflight", return_value=str(self.auth)), contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(asp.main(args), 0)
            self.assertEqual(asp.main(["--setup-only", "--kit-directory", str(self.root)]), 0)
            config = self.home / ".config/unreal-agent-pilot/asp/oauth-v1/runtime.json"
            before = config.read_bytes(), config.stat().st_mtime_ns
            with contextlib.redirect_stderr(io.StringIO()):
                self.assertEqual(asp.main([*args, "--model", "different-model"]), 1)
            self.assertEqual((config.read_bytes(), config.stat().st_mtime_ns), before)
            self.assertEqual(config.stat().st_mode & 0o777, 0o600)
            self.assertEqual(config.parent.stat().st_mode & 0o777, 0o700)
            self.assertFalse((self.home / ".config/unreal-agent/runtime.json").exists())
            self.assertFalse((self.home / ".local/state/unreal-agent").exists())

    def test_normal_launcher_and_no_automatic_smoke(self):
        with patch.dict(os.environ, self.env, clear=True), patch.object(asp, "validate_kit", return_value=self.root), patch.object(asp, "preflight", return_value=str(self.auth)), patch.object(asp.os, "execv") as execute, patch.object(pilot, "smoke", side_effect=AssertionError("automatic inference")), contextlib.redirect_stdout(io.StringIO()):
            asp.main(["--model", "gpt-6.1-sol", "--kit-directory", str(self.root), "chosen-session"])
        command = execute.call_args.args[1]
        self.assertEqual(command[0], str(self.root / "unreal"))
        self.assertEqual(command[-1], "chosen-session")
        self.assertIn(str(self.home / ".local/state/unreal-agent-pilot/asp/oauth-v1/run/host.sock"), command)
        self.assertIn(str(self.auth), command)

    def test_invalid_existing_config_is_kept_and_raw_values_withheld(self):
        directory = self.home / ".config/unreal-agent-pilot/asp/oauth-v1"
        directory.mkdir(parents=True, mode=0o700)
        path = directory / "runtime.json"
        for content in ('["PRIVATE_DUMMY"]', '{"Runtime":["PRIVATE_DUMMY"]}', '{"Runtime":{"Provider":[]}}', 'PRIVATE_DUMMY malformed'):
            path.write_text(content); path.chmod(0o600)
            with patch.dict(os.environ, self.env, clear=True), patch.object(asp, "validate_kit", return_value=self.root), patch.object(asp, "preflight", return_value=str(self.auth)), contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()) as output:
                self.assertEqual(asp.main(["--setup-only", "--kit-directory", str(self.root)]), 1)
            self.assertNotIn("PRIVATE_DUMMY", output.getvalue())
            self.assertEqual(path.read_text(), content)

    def test_env_optin_preserves_legacy_and_no_host_in_wrong_mode(self):
        with self.assertRaises(pilot.PilotError):
            pilot.ensure_host(self.root, {}, str(self.auth), {"SBX_CRED_OPENAI_MODE": "apikey"}, docker_sandbox_oauth=True)
        # No lock/log/start occurred before the refusal.
        self.assertFalse((self.root / "provider-start.lock").exists())

    def test_kit_has_new_revision_safe_extras_and_activation_syntax(self):
        revision = "a" * 40
        source = source_archive(self.root, revision=revision)
        output = self.root / "output"; output.mkdir()
        with patch.object(pilot_kit, "binary_requirements", return_value="2.34"):
            archive = asp_oauth_bundle.assemble(source, output, revision)
        package = output / "unreal-agent-asp-oauth-aaaaaaa"
        manifest = json.loads((package / "KIT.json").read_text())
        self.assertEqual(manifest["baseline"], revision)
        self.assertEqual(manifest["kind"], "asp-oauth-kit")
        for name in ("install.sh", "doctor.sh", "activate.sh"):
            result = subprocess.run(["bash", "-n", str(package / name)], capture_output=True)
            self.assertEqual(result.returncode, 0, result.stderr)
        names = {p.name for p in package.iterdir()}
        self.assertEqual(names, set(pilot_kit.FILES) | {"SHA256SUMS", "activate.sh", "asp_oauth.py", "pilot_provider.py"})
        self.assertNotIn("auth.json", names)
        self.assertNotIn("config.toml", names)
        subprocess.run(["sha256sum", "--check", "SHA256SUMS"], cwd=package, check=True, capture_output=True)
        self.assertTrue(archive.is_file())


@unittest.skipUnless(os.environ.get("UNREAL_ASP_OAUTH_KIT"), "set UNREAL_ASP_OAUTH_KIT for native kit installation/preflight/Host tests")
class AspOAuthNativeKitTests(unittest.TestCase):
    def setUp(self):
        self.kit = Path(os.environ["UNREAL_ASP_OAUTH_KIT"])
        self.temp = tempfile.TemporaryDirectory(prefix="ao-", dir="/tmp")
        self.addCleanup(self.temp.cleanup)
        self.home = Path(self.temp.name)
        (self.home / "workspace").mkdir()
        codex = self.home / ".codex"; codex.mkdir()
        for name, content in {
            "auth.json": '{"OPENAI_API_KEY":"proxy-managed"}',
            "config.toml": 'forced_login_method="api"\nmodel_provider="sandboxd"\n[model_providers.sandboxd]\nname="Sandbox Proxy"\nbase_url="https://chatgpt.com/backend-api/codex"\nexperimental_bearer_token="oai-oat01-proxy-managed"\nrequires_openai_auth=false\n',
            "models_cache.json": '{"models":[{"slug":"gpt-6.1-sol","visibility":"list","default_reasoning_level":"medium","supported_reasoning_levels":[{"effort":"medium"}]}]}',
        }.items():
            (codex / name).write_text(content); (codex / name).chmod(0o644)
        self.env = {"PATH": os.environ["PATH"], "HOME": str(self.home), "SBX_CRED_OPENAI_MODE": "oauth", "HTTPS_PROXY": "http://host.docker.internal:3128", "PYTHONDONTWRITEBYTECODE": "1"}
        self.before = {path: (path.read_bytes(), path.stat().st_mtime_ns) for path in codex.iterdir()}

    def run_driver(self, *args, env=None):
        return subprocess.run(["python3", str(self.kit / "asp_oauth.py"), "--kit-directory", str(self.kit), *args], env=env or self.env, capture_output=True, text=True, timeout=45)

    def test_install_rerun_activate_and_original_codex_unchanged(self):
        codex_cli = self.home / "codex-existing"; codex_cli.write_text("unchanged dummy CLI")
        for _ in range(2):
            result = subprocess.run(["bash", str(self.kit / "install.sh")], env=self.env, capture_output=True, text=True, timeout=60)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        result = subprocess.run(["bash", "-c", 'prior=$PATH; source "$1/activate.sh" --model gpt-6.1-sol --instance oauth-test && unreal --setup-only && test "$PATH" = "$prior" && unset -f unreal', "activate-test", str(self.kit)], env=self.env, capture_output=True, text=True, timeout=60)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(codex_cli.read_text(), "unchanged dummy CLI")
        self.assertEqual({p: (p.read_bytes(), p.stat().st_mtime_ns) for p in self.before}, self.before)

    def test_setup_no_auth_rewrite_and_api_key_mode_refused(self):
        result = self.run_driver("--setup-only", "--model", "gpt-6.1-sol")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        again = self.run_driver("--setup-only")
        self.assertEqual(again.returncode, 0, again.stdout + again.stderr)
        self.assertEqual({p: (p.read_bytes(), p.stat().st_mtime_ns) for p in self.before}, self.before)
        denied = self.run_driver("--setup-only", env=dict(self.env, SBX_CRED_OPENAI_MODE="apikey"))
        self.assertNotEqual(denied.returncode, 0)
        self.assertIn("docker_sandbox_oauth_unavailable", denied.stderr)
        for proxy in ("", "http://PRIVATE_DUMMY:secret@host.docker.internal:3128", "http://untrusted.invalid:3128"):
            denied = self.run_driver("--setup-only", env=dict(self.env, HTTPS_PROXY=proxy))
            self.assertNotEqual(denied.returncode, 0)
            self.assertIn("docker_sandbox_proxy_unavailable", denied.stderr)
            self.assertNotIn("PRIVATE_DUMMY", denied.stdout + denied.stderr)

    def test_real_binary_idle_host_catalog_session_and_drain_no_inference(self):
        result = self.run_driver("--setup-only", "--model", "gpt-6.1-sol")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        state = self.home / ".local/state/unreal-agent-pilot/asp/oauth-v1"
        paths = {"home": self.home, "config": self.home / ".config/unreal-agent-pilot/asp/oauth-v1/runtime.json", "state": state, "sessions": state / "sessions", "socket": state / "run/host.sock"}
        process = subprocess.Popen(pilot.host_arguments(self.kit, paths, str(self.home / ".codex/auth.json")), env=self.env, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        gateway = pilot.Gateway(paths["socket"])
        try:
            import time
            deadline = time.monotonic() + 20
            while time.monotonic() < deadline:
                self.assertIsNone(process.poll(), "native Host exited before readiness")
                try:
                    gateway.call("auth/methods", {}); break
                except (OSError, http.client.HTTPException): time.sleep(0.1)
            else: self.fail("native Host not ready")
            view = gateway.call("open", {"ID": "asp-offline", "Mode": "create"})["View"]
            self.assertTrue(view["Running"])
            self.assertFalse(view.get("Operations", []))
            self.assertTrue(all(item.get("Kind") not in ("model_response", "tool_call_status") for item in view.get("History", {}).get("Items", [])))
            model = gateway.call("extension", {"Action": "model.catalog", "ID": "asp-offline"})
            self.assertTrue(model["Catalog"]["Available"])
            self.assertEqual(model["Catalog"]["Models"][0]["ID"], "gpt-6.1-sol")
        finally:
            process.terminate(); process.wait(timeout=10)
        self.assertEqual(process.returncode, 0)
        self.assertTrue(paths["sessions"].is_dir())

    def test_update_instance_missing_cache_model_list_and_old_history_preserved(self):
        old_before = {}
        for instance in ('oauth-v1', 'oauth-catalog-v1'):
            old = self.home / '.local/state/unreal-agent-pilot/asp' / instance / 'sessions'
            old.mkdir(parents=True)
            marker = old / 'existing-history'; marker.write_bytes(b'immutable synthetic history')
            old_before[marker] = (marker.read_bytes(), marker.stat().st_mtime_ns)
        (self.home / ".codex/models_cache.json").unlink()  # custom providers need not write this cache
        binary_dir = self.home / "fake-cli"; binary_dir.mkdir()
        codex = binary_dir / "codex"
        codex.write_text('''#!/usr/bin/env python3
import json, pathlib, sys
requests = []
for line in sys.stdin:
    frame = json.loads(line); requests.append(frame['method'])
    if frame['method'] == 'initialize': print(json.dumps({'id':frame['id'],'result':{'userAgent':'fixture'}}), flush=True)
    elif frame['method'] == 'initialized': continue
    elif frame['method'] == 'model/list':
        data=[{'id':m,'model':m,'displayName':m,'hidden':False,'defaultReasoningEffort':'high','supportedReasoningEfforts':[{'reasoningEffort':'high'},{'reasoningEffort':'xhigh'}]} for m in ['fixture-a','fixture-b']]
        print(json.dumps({'id':frame['id'],'result':{'data':data,'nextCursor':None}}), flush=True)
    else: sys.exit(9)
''')
        codex.chmod(0o700)
        env = dict(self.env, PATH=str(binary_dir)+os.pathsep+self.env['PATH'])
        result = self.run_driver('--setup-only', '--model', 'gpt-6.1-sol', '--instance', 'oauth-inference-v1', env=env)
        self.assertEqual(result.returncode, 0, result.stdout+result.stderr)
        state = self.home / '.local/state/unreal-agent-pilot/asp/oauth-inference-v1'
        paths = {'home':self.home,'config':self.home/'.config/unreal-agent-pilot/asp/oauth-inference-v1/runtime.json','state':state,'sessions':state/'sessions','socket':state/'run/host.sock'}
        process = subprocess.Popen(pilot.host_arguments(self.kit, paths, str(self.home/'.codex/auth.json')), env=env, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        gateway = pilot.Gateway(paths['socket'])
        try:
            import time
            deadline = time.monotonic()+20
            while time.monotonic()<deadline:
                self.assertIsNone(process.poll(), 'updated native Host exited')
                try: gateway.call('auth/methods', {}); break
                except (OSError, http.client.HTTPException): time.sleep(0.1)
            else: self.fail('updated native Host not ready')
            gateway.call('open', {'ID':'catalog-no-inference','Mode':'create'})
            reply=gateway.call('extension', {'Action':'model.catalog','ID':'catalog-no-inference'})
            self.assertTrue(reply['Catalog']['Available'])
            self.assertEqual([m['ID'] for m in reply['Catalog']['Models']], ['fixture-a','fixture-b'])
            self.assertEqual(reply['Catalog']['Models'][1]['Efforts'], ['high','xhigh'])
            view=gateway.call('inspect', {'ID':'catalog-no-inference','Limit':256})['View']
            self.assertFalse(view.get('Operations', []))
            self.assertTrue(all(item.get('Kind') not in ('model_response','tool_call_status') for item in view.get('History',{}).get('Items',[])))
        finally:
            process.terminate(); process.wait(timeout=10)
        self.assertEqual(process.returncode, 0)
        self.assertEqual({marker: (marker.read_bytes(), marker.stat().st_mtime_ns) for marker in old_before}, old_before)
        for path, before in self.before.items():
            if path.name!='models_cache.json': self.assertEqual((path.read_bytes(),path.stat().st_mtime_ns),before)


if __name__ == "__main__":
    unittest.main()

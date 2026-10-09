import contextlib
import hashlib
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
import devlore_launcher as launcher
import devlore_launcher_bundle as bundle
import pilot_provider as pilot


class DevloreLauncherTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="dv-", dir="/tmp")
        self.addCleanup(self.temp.cleanup)
        self.home = Path(self.temp.name)
        (self.home / "workspace").mkdir()
        output = self.home / "kit-output"; output.mkdir()
        self.archive = bundle.build(output, "a" * 40)
        self.package = output / "unreal-agent-devlore-launcher-aaaaaaa"
        self.kit = self.home / "existing-native"; self.kit.mkdir()
        self.env = {"HOME": str(self.home), "PATH": "/usr/bin:/bin", "PYTHONDONTWRITEBYTECODE": "1"}
        self.args = ["install", "--package", str(self.package), "--kit-directory", str(self.kit),
                     "--policy-audited", "--trust-managed-policy"]

    def install(self, args=None, env=None):
        with patch.dict(os.environ, env or self.env, clear=True), \
                patch.object(pilot, "installed_kit", return_value=self.kit), \
                patch.object(pilot, "claude_preflight", return_value="/unused/claude") as preflight, \
                contextlib.redirect_stdout(io.StringIO()) as output, contextlib.redirect_stderr(io.StringIO()) as errors:
            result = launcher.main(args or self.args)
        return result, output.getvalue(), errors.getvalue(), preflight

    def require_success(self, result):
        self.assertEqual(result[:1] + result[2:3], (0, ""))

    def test_new_interactive_and_login_bash_path_and_readonly_config(self):
        # Preserve prefixes and Bash's existing login-file precedence.
        rc = self.home / ".bashrc"; rc.write_bytes(b"# keep existing non-login startup\n")
        login = self.home / ".bash_profile"; login.write_bytes(b"# keep custom login startup\n")
        original = {rc: rc.read_bytes(), login: login.read_bytes()}
        self.require_success(self.install())
        command = self.home / ".local/bin/unreal"
        for flags in (("--noprofile", "-ic"), ("-lic",)):
            result = subprocess.run(["/bin/bash", *flags, 'found=$(command -v unreal) && printf "\\nCOMMAND_PATH=%s\\n" "$found"'], env=self.env, capture_output=True, text=True, timeout=15)
            self.assertEqual(result.returncode, 0)
            self.assertEqual([line for line in result.stdout.splitlines() if line.startswith("COMMAND_PATH=")], ["COMMAND_PATH=" + str(command)])
        for path, prefix in original.items():
            self.assertTrue(path.read_bytes().startswith(prefix))
            self.assertEqual(path.read_text().count(launcher.PATH_MARKER), 1)
        self.assertFalse((self.home / ".profile").exists())
        config = self.home / ".config/unreal-agent-pilot/devlore/launcher-v1/runtime.json"
        data = json.loads(config.read_bytes())
        self.assertEqual(data["Runtime"]["Provider"]["provider"], "claude-code")
        self.assertEqual(data["Runtime"]["ClaudeCode"]["managedPolicyMode"], "trust")
        self.assertEqual(data["Runtime"]["ClaudeCode"]["ToolBridge"]["Mode"], "structured")
        self.assertEqual(data["Permissions"]["Tools"], ["read", "grep", "glob"])
        self.assertEqual(data["Permissions"]["WriteRoots"], [])
        self.assertEqual(data["Permissions"]["ProcessMode"], "")
        self.assertFalse(data["Permissions"]["FilesystemUnrestricted"])
        self.assertFalse(data["Permissions"]["NetworkUnrestricted"])
        self.assertEqual(data["Runtime"]["Workspace"], str(self.home / "workspace"))
        self.assertEqual(config.stat().st_mode & 0o777, 0o600)
        self.assertEqual(config.parent.stat().st_mode & 0o777, 0o700)
        self.assertEqual(command.stat().st_mode & 0o777, 0o700)

    def test_rerun_preserves_config_command_profiles_auth_and_old_sessions(self):
        originals = []
        for name in (".claude/credentials", ".claude/settings.json", ".config/unreal-agent/runtime.json",
                     ".local/state/unreal-agent/sessions/existing", ".local/state/unreal-agent-pilot/devlore/v1/sessions/old-smoke",
                     ".local/state/unreal-agent-pilot/asp/oauth-inference-v1/sessions/asp-existing"):
            path = self.home / name; path.parent.mkdir(parents=True, exist_ok=True); path.write_bytes(b"PRIVATE_DUMMY_KEEP")
            originals.append(path)
        self.require_success(self.install())
        paths = originals + [self.home / name for name in (".bashrc", ".profile", ".local/bin/unreal",
            ".config/unreal-agent-pilot/devlore/launcher-v1/runtime.json", ".config/unreal-agent-pilot/devlore/launcher-v1/launcher.json")]
        before = {path: (path.read_bytes(), path.stat().st_mtime_ns) for path in paths}
        self.require_success(self.install())
        self.assertEqual({p: (p.read_bytes(), p.stat().st_mtime_ns) for p in paths}, before)
        self.assertFalse((self.home / ".local/state/unreal-agent-pilot/devlore/launcher-v1/sessions").exists())

    def test_existing_command_path_alias_function_and_redirect_refused(self):
        for kind in ("target", "path", "alias", "function", "symlink"):
            with self.subTest(kind=kind):
                target = self.home / ".local/bin/unreal"
                rc = self.home / ".bashrc"
                foreign = self.home / "foreign"; foreign.mkdir(exist_ok=True)
                old = foreign / "unreal"; old.write_bytes(b"#!/bin/bash\n# PRIVATE_DUMMY\n"); old.chmod(0o700)
                env = dict(self.env)
                if kind == "target":
                    target.parent.mkdir(parents=True, exist_ok=True); target.write_bytes(b"PRIVATE_DUMMY"); target.chmod(0o600)
                elif kind == "symlink":
                    target.symlink_to(old)
                elif kind == "path":
                    env["PATH"] = str(foreign) + ":" + env["PATH"]
                elif kind == "alias":
                    rc.write_text("alias unreal='echo PRIVATE_DUMMY'\n")
                else:
                    rc.write_text("unreal() { echo PRIVATE_DUMMY; }\n")
                before = {p: p.read_bytes() for p in (old, rc) if p.exists()}
                result = self.install(env=env)
                self.assertNotEqual(result[0], 0)
                result[3].assert_not_called()
                self.assertNotIn("PRIVATE_DUMMY", result[1] + result[2])
                self.assertEqual({p: p.read_bytes() for p in before}, before)
                if os.path.lexists(target): target.unlink()  # owned synthetic fixture only
                if rc.exists(): rc.unlink()

    def test_missing_attestation_stops_before_provider_or_publication(self):
        for flag in ("--policy-audited", "--trust-managed-policy"):
            result = self.install(args=[arg for arg in self.args if arg != flag])
            self.assertNotEqual(result[0], 0)
            self.assertIn("managed_policy_audit_required", result[2])
            result[3].assert_not_called()
            self.assertFalse((self.home / ".config").exists())
            self.assertFalse((self.home / ".local/bin/unreal").exists())

    def test_claude_missing_or_unauthenticated_stops_without_publication(self):
        for code in ("claude_binary_missing", "claude_login_required", "claude_contract_unavailable"):
            with self.subTest(code=code), patch.dict(os.environ, self.env, clear=True), \
                    patch.object(pilot, "installed_kit", return_value=self.kit), \
                    patch.object(pilot, "claude_preflight", side_effect=pilot.PilotError(code, "Use existing CLI separately.")), \
                    contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()) as errors:
                self.assertEqual(launcher.main(self.args), 1)
                self.assertIn(code, errors.getvalue())
                self.assertFalse((self.home / ".config").exists())
                self.assertFalse((self.home / ".local/bin/unreal").exists())
                self.assertFalse((self.home / ".bashrc").exists())
                self.assertFalse((self.home / ".profile").exists())

    def test_path_already_present_does_not_edit_profiles_or_directory_mode(self):
        directory = self.home / ".local/bin"; directory.mkdir(parents=True, mode=0o755); directory.chmod(0o755)
        env = dict(self.env, PATH=str(directory) + ":" + self.env["PATH"])
        self.require_success(self.install(env=env))
        self.assertEqual(directory.stat().st_mode & 0o777, 0o755)
        self.assertFalse((self.home / ".bashrc").exists())
        self.assertFalse((self.home / ".profile").exists())

    def test_profile_symlink_unsafe_permissions_and_existing_config_kept(self):
        profile = self.home / ".bashrc"
        original = self.home / "keep-profile"; original.write_bytes(b"PRIVATE_DUMMY_KEEP")
        profile.symlink_to(original)
        result = self.install(); self.assertNotEqual(result[0], 0); result[3].assert_not_called()
        self.assertEqual(original.read_bytes(), b"PRIVATE_DUMMY_KEEP")
        profile.unlink(); profile.write_bytes(b"PRIVATE_DUMMY_KEEP"); profile.chmod(0o666)
        result = self.install(); self.assertNotEqual(result[0], 0); result[3].assert_not_called()
        self.assertEqual(profile.stat().st_mode & 0o777, 0o666)
        profile.chmod(0o600)
        self.require_success(self.install())
        config = self.home / ".config/unreal-agent-pilot/devlore/launcher-v1/runtime.json"
        data = config.read_bytes(); config.write_bytes(b"PRIVATE_DUMMY_CHANGED"); before = config.stat().st_mtime_ns
        result = self.install(); self.assertNotEqual(result[0], 0)
        self.assertEqual((config.read_bytes(), config.stat().st_mtime_ns), (b"PRIVATE_DUMMY_CHANGED", before))
        self.assertNotIn("PRIVATE_DUMMY", result[1] + result[2])

    def test_profile_output_withheld_and_argument_paths_quoted(self):
        rc = self.home / ".bashrc"; rc.write_text("printf 'PRIVATE_DUMMY\\n'\n")
        result = self.install(); self.require_success(result)
        self.assertNotIn("PRIVATE_DUMMY", result[1] + result[2])
        wrapper = self.home / ".local/bin/unreal"
        subprocess.run(["bash", "-n", str(wrapper)], check=True)
        self.assertIn(b'"$@"', wrapper.read_bytes())
        self.assertNotIn(b"eval ", wrapper.read_bytes())

    def test_launch_delegates_native_lifecycle_and_never_submits_smoke(self):
        self.require_success(self.install())
        prior = os.getcwd()
        self.addCleanup(os.chdir, prior)
        with patch.dict(os.environ, self.env, clear=True), patch.object(pilot, "installed_kit", return_value=self.kit), \
                patch.object(launcher.os, "execv") as execute, patch.object(pilot, "smoke", side_effect=AssertionError("automatic inference")):
            self.assertEqual(launcher.main(["launch"]), 0)
            command = execute.call_args.args[1]
            self.assertEqual(command[0], str(self.kit / "unreal"))
            self.assertEqual(command[-1], "default")
            self.assertIn(str(self.home / ".local/state/unreal-agent-pilot/devlore/launcher-v1/run/host.sock"), command)
            self.assertNotIn("--codex-auth-file", command)
            self.assertEqual(launcher.main(["launch", "fresh-dev"]), 0)
            self.assertEqual(execute.call_args.args[1][-1], "fresh-dev")
            self.assertEqual(os.getcwd(), str(self.home / "workspace"))

    def test_package_inventory_checksums_two_steps_and_no_binary_auth(self):
        self.assertEqual(launcher.validate_package(self.package), hashlib.sha256((self.package / "SHA256SUMS").read_bytes()).hexdigest()[:16])
        with tarfile.open(self.archive) as archive:
            self.assertEqual({Path(member.name).name for member in archive}, set(launcher.MEMBERS) | {"SHA256SUMS"})
            self.assertTrue(all(member.isfile() for member in archive))
        text = (self.package / "README.md").read_text()
        self.assertEqual([line[:2] for line in text.splitlines() if line[:2] in ("1.", "2.", "3.")], ["1.", "2."])
        self.assertIn("sbx cp", text)
        self.assertNotIn("@ARCHIVE@", text)
        subprocess.run(["bash", "-n", str(self.package / "install.sh")], check=True)
        (self.package / "pilot_provider.py").write_bytes(b"PRIVATE_DUMMY_MODIFIED")
        with self.assertRaises(pilot.PilotError) as failure:
            launcher.validate_package(self.package)
        self.assertEqual(failure.exception.code, "launcher_checksum_mismatch")
        self.assertNotIn("PRIVATE_DUMMY", str(failure.exception))


if __name__ == "__main__":
    unittest.main()

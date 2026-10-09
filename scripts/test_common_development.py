import argparse
import contextlib
import hashlib
import io
import json
import os
from pathlib import Path
import pty
import select
import shutil
import subprocess
import sys
import tarfile
import tempfile
import time
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).parent))
import common_development as common
import common_development_bundle as bundle
import daily_development as daily
import pilot_kit
import pilot_provider as pilot
from test_pilot_kit import source_archive


class CommonDevelopmentTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="uc-", dir="/tmp")
        self.addCleanup(self.temp.cleanup)
        self.home = Path(self.temp.name)
        self.env = {"HOME": str(self.home), "PATH": "/usr/bin:/bin", "PYTHONDONTWRITEBYTECODE": "1"}
        output = self.home / "output"; output.mkdir()
        with patch.object(pilot_kit, "binary_requirements", return_value="2.34"):
            self.archive = bundle.assemble(source_archive(self.home, revision="a" * 40), output, "a" * 40)
        self.package = output / "unreal-agent-linux-amd64-aaaaaaa"
        self.meta = json.loads((self.package / "KIT.json").read_bytes())
        self.args = argparse.Namespace(docker_sandbox_oauth=False, policy_audited=False,
            trust_managed_policy=False, claude_binary="", replace_pilot_launcher=False)

    def install(self):
        def probe(argv, env, **kwargs):
            if argv == ["bash", str(self.package / "install.sh")]:
                identity = hashlib.sha256((self.package / "SHA256SUMS").read_bytes()).hexdigest()[:16]
                installed = self.home / ".local/opt/unreal-agent-pilot/common" / ("aaaaaaa-" + identity)
                if not installed.exists(): shutil.copytree(self.package, installed)
                return b"", 0
            return original(argv, env, **kwargs)
        original = pilot.bounded_probe
        with patch.dict(os.environ, self.env, clear=True), patch.object(pilot, "bounded_probe", side_effect=probe), contextlib.redirect_stdout(io.StringIO()):
            common.install(self.args, self.package, self.meta)
        return common.read_settings(self.home)

    def repo(self, name="workspace/project"):
        root = self.home / name; root.mkdir(parents=True)
        subprocess.run(["git", "init", "--quiet", str(root)], check=True, env=self.env)
        return root

    def test_one_inventory_same_product_no_credentials(self):
        with tarfile.open(self.archive) as archive:
            self.assertEqual({p.name for p in archive}, {self.package.name + "/" + n for n in (*common.MEMBERS, "SHA256SUMS")})
            self.assertTrue(all(p.isfile() for p in archive))
        self.assertNotIn("sandbox", self.meta)
        self.assertNotIn("Model", self.meta)
        self.assertEqual(common.package(self.package), self.meta)
        subprocess.run(["bash", "-n", str(self.package / "development.sh")], check=True)
        subprocess.run(["bash", "-n", str(self.package / "install.sh")], check=True)
        self.assertNotIn("DiscoverProviders", (self.package / "common_development.py").read_text())

    def test_install_idempotent_fresh_bash_and_no_auth_import(self):
        sentinel = self.home / ".codex/auth.json"; sentinel.parent.mkdir()
        sentinel.write_bytes(b"PRIVATE_DO_NOT_READ"); sentinel.chmod(0o600)
        old_state = self.home / ".local/state/unreal-agent-pilot/devlore/gate/session"; old_state.parent.mkdir(parents=True)
        old_state.write_bytes(b"KEEP_HISTORY")
        s = self.install()
        first = {p: p.read_bytes() for p in (common.settings_path(self.home), self.home / ".local/bin/unreal", self.home / ".bashrc", self.home / ".profile")}
        self.install()
        for p, data in first.items(): self.assertEqual(p.read_bytes(), data)
        self.assertEqual(sentinel.read_bytes(), b"PRIVATE_DO_NOT_READ")
        self.assertEqual(old_state.read_bytes(), b"KEEP_HISTORY")
        self.assertFalse(s["TrustManagedPolicy"])
        self.assertNotIn("Model", s); self.assertNotIn("Effort", s)
        self.assertEqual(common.settings_path(self.home).stat().st_mode & 0o777, 0o600)
        self.assertEqual(common.settings_path(self.home).parent.stat().st_mode & 0o777, 0o700)
        for flags in (("--noprofile", "-ic"), ("-lic",)):
            result = subprocess.run(["bash", *flags, 'printf "\\nCOMMON_COMMAND:%s\\n" "$(type -P unreal)"'], env=self.env, capture_output=True, text=True, timeout=15)
            self.assertEqual(result.returncode, 0)
            rows = [line for line in result.stdout.splitlines() if line.startswith("COMMON_COMMAND:")]
            self.assertEqual(rows, ["COMMON_COMMAND:" + str(self.home / ".local/bin/unreal")])

    def test_unrelated_command_alias_function_and_symlink_preserved(self):
        for kind in ("command", "symlink", "alias", "function"):
            with self.subTest(kind=kind):
                if kind in ("command", "symlink"):
                    folder = self.home / ".local/bin"; folder.mkdir(parents=True, exist_ok=True)
                    command = folder / "unreal"
                    if kind == "command": command.write_bytes(b"#!/bin/bash\nexit 0\n"); command.chmod(0o700)
                    else: command.symlink_to(self.package / "unreal")
                else:
                    (self.home / ".bashrc").write_text("alias unreal='echo private'\n" if kind == "alias" else "unreal() { echo private; }\n")
                self.args.replace_pilot_launcher = True
                with self.assertRaises((pilot.PilotError, OSError)): self.install()
                self.assertFalse(common.settings_path(self.home).exists())
                if kind in ("command", "symlink"): command.unlink()
                else: (self.home / ".bashrc").unlink()

    def test_owned_update_requires_optin_private_backup_and_config_preservation(self):
        s = self.install(); root = self.repo()
        p = common.prepare_repo(s, root, "default")
        config = {"Runtime": {"Workspace": str(root)}, "Launcher": {"Version": 1}}
        p["config"].parent.mkdir(parents=True, exist_ok=True)
        p["config"].write_text(json.dumps(config)); p["config"].chmod(0o600)
        original = p["config"].read_bytes()
        self.args.docker_sandbox_oauth = True
        with self.assertRaises(pilot.PilotError): self.install()
        self.args.replace_pilot_launcher = True
        new = self.install()
        self.assertTrue(new["DockerOAuth"])
        self.assertEqual(p["config"].read_bytes(), original)
        backups = common.settings_path(self.home).parent / "backups"
        self.assertTrue(list(backups.glob("*.json")))
        for f in backups.iterdir(): self.assertEqual(f.stat().st_mode & 0o777, 0o600)

    def test_managed_trust_is_explicit_pair_before_any_subprocess(self):
        for audited, trusted in ((True, False), (False, True)):
            self.args.policy_audited, self.args.trust_managed_policy = audited, trusted
            with patch.dict(os.environ, self.env, clear=True), patch.object(pilot, "bounded_probe") as probe, self.assertRaises(pilot.PilotError):
                common.install(self.args, self.package, self.meta)
            probe.assert_not_called()
        self.args.policy_audited = self.args.trust_managed_policy = True
        s = self.install()
        self.assertTrue(s["PolicyAudited"] and s["TrustManagedPolicy"])

    def test_exact_legacy_daily_wrapper_requires_optin_and_keeps_history(self):
        old_directory = self.home / "old-kit"
        old = daily.wrapper(old_directory)
        settings = {"Version": 1, "Sandbox": "asp", "Home": str(self.home),
                    "BinaryDirectory": str(old_directory), "Revision": "b" * 40,
                    "Model": "gpt-6.1-sol", "Effort": "high",
                    "CodexFile": str(self.home / ".codex/auth.json"), "ClaudeBinary": "",
                    "PolicyAudited": False, "TrustManagedPolicy": False,
                    "CommandHash": hashlib.sha256(old).hexdigest()}
        daily.settings_path(self.home, "asp").parent.mkdir(parents=True, mode=0o700)
        pilot.publish_json(daily.settings_path(self.home, "asp"), settings)
        command = self.home / ".local/bin/unreal"; command.parent.mkdir(parents=True)
        command.write_bytes(old); command.chmod(0o700)
        history = self.home / ".local/state/unreal-agent-dev/asp/old-session"
        history.parent.mkdir(parents=True); history.write_bytes(b"KEEP_OLD_CANONICAL_HISTORY")
        before = {p: (p.read_bytes(), p.stat().st_mtime_ns) for p in (history, daily.settings_path(self.home, "asp"))}
        with self.assertRaises(pilot.PilotError): self.install()
        self.assertEqual(command.read_bytes(), old)
        self.args.replace_pilot_launcher = True
        self.install()
        self.assertNotEqual(command.read_bytes(), old)
        backup = common.settings_path(self.home).parent / "backups" / (hashlib.sha256(old).hexdigest() + ".sh")
        self.assertEqual(backup.read_bytes(), old)
        self.assertEqual({p: (p.read_bytes(), p.stat().st_mtime_ns) for p in before}, before)

    def test_quickstart_pty_waits_for_explicit_enter_without_host_or_expiry(self):
        (self.package / "unreal").write_bytes(b'#!/bin/bash\nprintf invoked > "$HOME/native-started"\n')
        (self.package / "SHA256SUMS").write_bytes(b"".join(hashlib.sha256((self.package / name).read_bytes()).hexdigest().encode()
                                                            + b"  " + name.encode() + b"\n" for name in common.MEMBERS))
        self.install(); root = self.repo()
        master, slave = pty.openpty()
        process = subprocess.Popen(["python3", "-B", str(self.package / "common_development.py"), "launch", "--instance", "read-instructions", "dev"],
                                   cwd=root, env=self.env, stdin=slave, stdout=slave, stderr=slave)
        os.close(slave)
        self.addCleanup(os.close, master)
        def reap():
            if process.poll() is None: process.terminate()
            process.wait(timeout=10)
        self.addCleanup(reap)
        output = b""; deadline = time.monotonic() + 10
        while "[Enterで起動]".encode() not in output and time.monotonic() < deadline:
            if select.select([master], [], [], 0.1)[0]: output += os.read(master, 4096)
        self.assertIn("[Enterで起動]".encode(), output)
        self.assertNotIn(b"# Unreal Agent", output, "detailed README must not display automatically")
        p = common.repo_paths(self.home, root, "read-instructions")
        self.assertFalse(p["config"].exists())
        self.assertFalse(p["socket"].exists())
        self.assertFalse((self.home / "native-started").exists())
        self.assertIsNone(process.poll(), "real Enter is required, not a generated approval")
        # The synthetic native executable exits 0. Before Enter it cannot run.
        os.write(master, b"\n")
        self.assertEqual(process.wait(timeout=10), 0)
        self.assertFalse(p["config"].exists())
        self.assertEqual((self.home / "native-started").read_bytes(), b"invoked")

    def test_repo_worktree_and_instance_isolation_no_runtime_duplication(self):
        s = self.install(); root = self.repo("workspace/日本語 project")
        p = common.prepare_repo(s, root, "default")
        self.assertFalse(p["config"].exists()) # native bootstrap is the only writer
        second = common.prepare_repo(s, self.repo("workspace/other"), "default")
        self.assertNotEqual(p["socket"], second["socket"])
        third = common.prepare_repo(s, root, "repair-v2")
        self.assertNotEqual(p["socket"], third["socket"])
        self.assertNotEqual(p["config"], third["config"])
        (root / "keep").write_text("synthetic")
        subprocess.run(["git", "-C", str(root), "add", "keep"], check=True, env=self.env)
        subprocess.run(["git", "-C", str(root), "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "fixture"], check=True, env=self.env)
        worktree = self.home / "workspace/linked"
        subprocess.run(["git", "-C", str(root), "worktree", "add", "--quiet", "--detach", str(worktree)], check=True, env=self.env)
        self.assertNotEqual(p["socket"], common.prepare_repo(s, worktree, "default")["socket"])
        common.prepare_repo(s, root, "default")
        p["binding"].write_bytes(b"{}"); p["binding"].chmod(0o600)
        with self.assertRaises(pilot.PilotError): common.prepare_repo(s, root, "default")

    def test_launch_native_normal_args_no_submission_quickstart_no_hang(self):
        self.args.docker_sandbox_oauth = self.args.policy_audited = self.args.trust_managed_policy = True
        s = self.install(); root = self.repo()
        args = argparse.Namespace(session="dev", instance="default", readme=False, workspace_info=False, acceptance_fixture=False)
        with patch.dict(os.environ, self.env, clear=True), patch.object(daily, "git_root", return_value=root), patch.object(os, "chdir"), patch.object(os, "execve") as launch, patch("builtins.input") as prompt, contextlib.redirect_stdout(io.StringIO()):
            common.launch(args, self.package)
        prompt.assert_not_called()
        executable, argv, env = launch.call_args.args
        self.assertEqual(executable, str(Path(s["BinaryDirectory"]) / "unreal"))
        self.assertNotIn("--config", argv)
        self.assertNotIn("--model", argv); self.assertNotIn("--effort", argv)
        self.assertNotIn("--submit", argv)
        self.assertIn("--bootstrap-codex-docker-oauth", argv)
        self.assertIn("--bootstrap-claude-policy-audited", argv)
        self.assertIn("--bootstrap-claude-managed-trust", argv)
        self.assertEqual(argv[-1], "dev")
        self.assertTrue(env["XDG_CONFIG_HOME"].endswith("/native"))
        for mode in ("readme", "workspace_info"):
            setattr(args, mode, True)
            with patch.dict(os.environ, self.env, clear=True), patch.object(daily, "git_root", return_value=root), patch.object(os, "execve") as execute, patch("builtins.input") as prompt, contextlib.redirect_stdout(io.StringIO()):
                common.launch(args, self.package)
            execute.assert_not_called(); prompt.assert_not_called(); setattr(args, mode, False)

    def test_opaque_bad_metadata_and_package_redirect_are_not_accepted(self):
        with self.assertRaises(pilot.PilotError): common.repo_paths(self.home, self.home / "repo", "../old")
        (self.package / "KIT.json").write_text('{}')
        with self.assertRaises(pilot.PilotError): common.package(self.package)


@unittest.skipUnless(os.environ.get("UNREAL_COMMON_KIT_DIRECTORY"),
                     "set UNREAL_COMMON_KIT_DIRECTORY for final common-archive installer/doctor checks")
class CommonSnapshotTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="uc-package-", dir="/tmp")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.home = self.root / "home with spaces"; self.home.mkdir(mode=0o700)
        self.package = Path(os.environ["UNREAL_COMMON_KIT_DIRECTORY"]).resolve()
        self.env = {"HOME": str(self.home), "PATH": "/usr/bin:/bin", "LC_ALL": "C",
                    "PYTHONDONTWRITEBYTECODE": "1", "TMPDIR": str(self.root)}

    def run_script(self, name, *args):
        return subprocess.run(["bash", str(self.package / name), *args], env=self.env,
                              capture_output=True, text=True, timeout=120)

    def test_final_manifest_doctor_and_all_four_binaries(self):
        with patch.dict(os.environ, self.env, clear=True):
            meta = common.package(self.package)
        self.assertNotIn("sandbox", meta)
        result = self.run_script("doctor.sh")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        for name in daily.asp.pilot_binaries():
            self.assertIn(name + ": non-inference CLI", result.stdout)
        self.assertIn("network/API connectivity NOT probed", result.stdout)
        self.assertFalse((self.home / ".local").exists())

    def test_final_install_rerun_new_bash_no_credentials_or_store_changes(self):
        existing = {".codex/auth.json": b"PRIVATE_SYNTHETIC_KEEP_NO_IMPORT",
                    ".codex/config.toml": b"KEEP_CODEX_CONFIG",
                    ".config/unreal-agent/runtime.json": b"KEEP_NORMAL_RUNTIME",
                    ".local/state/unreal-agent-pilot/devlore/old/history": b"KEEP_HISTORY",
                    ".claude/credentials.fixture": b"KEEP_SYNTHETIC_CLAUDE"}
        facts = {}
        for name, data in existing.items():
            p = self.home / name; p.parent.mkdir(parents=True, exist_ok=True)
            p.write_bytes(data); p.chmod(0o600)
            facts[p] = (data, p.stat().st_mtime_ns)
        first = self.run_script("development.sh")
        self.assertEqual(first.returncode, 0, first.stdout + first.stderr)
        paths = (common.settings_path(self.home), self.home / ".local/bin/unreal", self.home / ".bashrc", self.home / ".profile")
        kept = {p: (p.read_bytes(), p.stat().st_mtime_ns) for p in paths}
        second = self.run_script("development.sh")
        self.assertEqual(second.returncode, 0, second.stdout + second.stderr)
        self.assertEqual({p: (p.read_bytes(), p.stat().st_mtime_ns) for p in paths}, kept)
        self.assertEqual({p: (p.read_bytes(), p.stat().st_mtime_ns) for p in facts}, facts)
        self.assertNotIn("PRIVATE_SYNTHETIC", first.stdout + first.stderr + second.stdout + second.stderr)
        for flags in (("--noprofile", "-ic"), ("-lic",)):
            result = subprocess.run(["bash", *flags, 'printf "\\nCOMMON_COMMAND:%s\\n" "$(type -P unreal)"; unreal --readme'],
                                    cwd=self.root, env=self.env, capture_output=True, text=True, timeout=30)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            self.assertIn("COMMON_COMMAND:" + str(self.home / ".local/bin/unreal"), result.stdout)
            self.assertIn("共通開発キット", result.stdout)
        self.assertFalse((self.home / ".local/state/unreal-agent-common").exists())

    def test_final_wrong_os_arch_glibc_and_loader_refused(self):
        shims = self.root / "shims"; shims.mkdir()
        self.env["PATH"] = str(shims) + ":/usr/bin:/bin"
        cases = (("uname", "echo Darwin"),
                 ("uname", 'if [ "$1" = -s ]; then echo Linux; else echo aarch64; fi'),
                 ("getconf", "echo 'glibc 2.1'"),
                 ("ldd", "echo 'libc.so.6 => not found'; exit 1"))
        for name, body in cases:
            with self.subTest(name=name, body=body):
                p = shims / name; p.write_text("#!/bin/sh\n" + body + "\n"); p.chmod(0o700)
                result = self.run_script("install.sh")
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse((self.home / ".local").exists())
                p.unlink()  # Only a deliberately invalid disposable fixture.


if __name__ == "__main__": unittest.main()

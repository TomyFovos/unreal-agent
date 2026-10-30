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
        self.preflight_calls = []
        self.args = argparse.Namespace(docker_sandbox_oauth=False, policy_audited=False,
            trust_managed_policy=False, claude_binary="", replace_pilot_launcher=False)

    def install(self):
        def probe(argv, env, **kwargs):
            if argv == ["bash", str(self.package / "install.sh")]:
                identity = hashlib.sha256((self.package / "SHA256SUMS").read_bytes()).hexdigest()[:16]
                installed = self.home / ".local/opt/unreal-agent-pilot/common" / ("aaaaaaa-" + identity)
                if not installed.exists(): shutil.copytree(self.package, installed)
                return b"", 0
            if len(argv)>1 and argv[1] == "providers":
                self.preflight_calls.append(list(argv))
                reply = {"Providers": [{"ID": "openai-codex", "Availability": "unavailable" if "--codex-disabled" in argv else "available", "Reason":"operator_disabled" if "--codex-disabled" in argv else ""}, {"ID":"claude-code", "Availability":"unavailable" if "--claude-disabled" in argv else "available", "Reason":"operator_disabled" if "--claude-disabled" in argv else ""}]}
                if "--codex-auto" in argv: reply["CodexRoute"] = "external"
                return json.dumps(reply).encode(),0
            return original(argv, env, **kwargs)
        original = pilot.bounded_probe
        self.install_output = io.StringIO()
        with patch.dict(os.environ, self.env, clear=True), patch.object(pilot, "bounded_probe", side_effect=probe), contextlib.redirect_stdout(self.install_output):
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
        source=(self.package / "common_development.py").read_text()
        # Inspecting owned provider flags is required for updates; constructing
        # runtime/authentication in Python remains forbidden.
        for forbidden in ('"Runtime":', 'auth.json', '.credentials'):
            self.assertNotIn(forbidden,source)

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
        self.assertEqual(s["CodexRoute"], "external")
        self.assertNotIn("Model", s); self.assertNotIn("Effort", s)
        self.assertEqual(common.settings_path(self.home).stat().st_mode & 0o777, 0o600)
        self.assertEqual(common.settings_path(self.home).parent.stat().st_mode & 0o777, 0o700)
        for flags in (("--noprofile", "-ic"), ("-lic",)):
            result = subprocess.run(["bash", *flags, 'printf "\\nCOMMON_COMMAND:%s\\n" "$(type -P unreal)"'], env=self.env, capture_output=True, text=True, timeout=15)
            self.assertEqual(result.returncode, 0)
            rows = [line for line in result.stdout.splitlines() if line.startswith("COMMON_COMMAND:")]
            self.assertEqual(rows, ["COMMON_COMMAND:" + str(self.home / ".local/bin/unreal")])

    def test_auto_preflight_never_accepts_disabled_or_unproven_route(self):
        settings = self.install()
        settings.update(CodexRoute="auto", DockerOAuth=False)
        for route, available in (("disabled", True), ("unexpected", True), ("auto", True),
                                 ("docker", False), ("external", False)):
            with self.subTest(route=route, available=available):
                reply = {"CodexRoute": route, "Providers": [
                    {"ID": "openai-codex", "Availability": "available" if available else "unavailable",
                     "Reason": "" if available else "auth_required"},
                    {"ID": "claude-code", "Availability": "available"}]}
                with patch.object(pilot, "bounded_probe", return_value=(json.dumps(reply).encode(), 0)), self.assertRaises(pilot.PilotError):
                    common.preflight(settings)

    def test_install_ucd_when_local_bin_already_on_path(self):
        self.env["PATH"] = str(self.home / ".local/bin") + ":/usr/bin:/bin"
        settings = self.install()
        workspace = self.home / "work space"; workspace.mkdir()
        paths = common.select_repo(settings, workspace, None)
        sessions = paths["state"] / "sessions"; sessions.mkdir(parents=True)
        (sessions / "dev.session.jsonl").write_bytes(b"not read by discovery")
        (sessions / "child-hidden.session.jsonl").write_bytes(b"not listed")
        for flags in (("--noprofile", "-ic"), ("-lic",)):
            result = subprocess.run(["bash", *flags, 'ucd dev && printf "\\nWORKSPACE:%s\\n" "$PWD"'],
                                    cwd=self.home, env=self.env, capture_output=True, text=True, timeout=15)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn("WORKSPACE:" + str(workspace), result.stdout)
            failed = subprocess.run(["bash", *flags, 'ucd missing; status=$?; printf "\\nAFTER:%s:%s\\n" "$status" "$PWD"'],
                                    cwd=self.home, env=self.env, capture_output=True, text=True, timeout=15)
            self.assertIn("AFTER:1:" + str(self.home), failed.stdout)

    def test_subagent_launcher_keeps_path_and_updates_target(self):
        settings = self.install()
        command = self.home / ".local/bin/unreal-agent"
        first = command.read_bytes()
        self.assertEqual(first, common.agent_wrapper(Path(settings["BinaryDirectory"])))
        # Publish a new kit identity through the actual install path.
        (self.package / "SHA256SUMS").write_bytes(b"new kit identity\n")
        updated = self.install()
        self.assertNotEqual(settings["BinaryDirectory"], updated["BinaryDirectory"])
        self.assertEqual(command.read_bytes(), common.agent_wrapper(Path(updated["BinaryDirectory"])))
        self.assertEqual(command.stat().st_mode & 0o777, 0o700)
        target = Path(updated["BinaryDirectory"]) / "unreal-agent"
        target.write_text('#!/bin/bash\nprintf "%s\\n" "$@"\n')
        target.chmod(0o700)
        result = subprocess.run([str(command), "subagent", "show", "child id"], capture_output=True, text=True, check=True)
        self.assertEqual(result.stdout.splitlines(), ["subagent", "show", "child id"])

    def test_install_keeps_current_previous_and_running_version(self):
        previous = Path(self.install()["BinaryDirectory"])
        base = previous.parent
        running = base / ("b" * 7 + "-" + "b" * 16)
        obsolete = base / ("c" * 7 + "-" + "c" * 16)
        running.mkdir(); obsolete.mkdir()
        (obsolete / "unused").write_bytes(b"obsolete kit\n" * 1000)
        freed = common.allocated_bytes(obsolete)
        script = running / "launcher.py"
        script.write_text("import time\ntime.sleep(60)\n")
        process = subprocess.Popen([sys.executable, str(script)])
        try:
            (self.package / "SHA256SUMS").write_bytes(b"updated kit\n")
            current = Path(self.install()["BinaryDirectory"])
            self.assertEqual({p for p in base.iterdir() if p.is_dir()}, {previous, running, current})
            self.assertIn(f"旧版を削除: {obsolete.name}（{freed / 1e6:.0f} MB 解放）", self.install_output.getvalue())
            self.assertEqual(common.read_settings(self.home)["PreviousBinaryDirectory"], str(previous))
            # Reinstalling the same kit must not discard its rollback version.
            self.install()
            self.assertTrue(previous.is_dir())
        finally:
            process.terminate()
            process.wait(timeout=5)

    def test_cleanup_preserves_executable_mapping_and_does_not_follow_symlinks(self):
        current = Path(self.install()["BinaryDirectory"])
        base = current.parent
        running = base / ("d" * 7 + "-" + "d" * 16)
        running.mkdir()
        executable = running / "sleeper"
        shutil.copy2(shutil.which("sleep"), executable)
        outside = self.home / "outside"; outside.mkdir()
        (outside / "keep").write_text("keep")
        alias = base / ("e" * 7 + "-" + "e" * 16)
        alias.symlink_to(outside, target_is_directory=True)
        # Supply a different argv[0], so only /proc/PID/exe identifies the kit.
        process = subprocess.Popen(["kit-sleeper", "60"], executable=str(executable))
        try:
            with contextlib.redirect_stdout(io.StringIO()):
                common.prune_versions(self.home, current, None)
            self.assertTrue(running.is_dir())
            self.assertTrue(alias.is_symlink())
            self.assertEqual((outside / "keep").read_text(), "keep")
        finally:
            process.terminate()
            process.wait(timeout=5)

    def test_previous_version_can_come_from_workspace_choice(self):
        installed = self.home / ".local/opt/unreal-agent-pilot/common/new"
        prior = installed.parent / "old"
        choice = self.home / ".config" / common.NAMESPACE / "repos/r-test/current.json"
        choice.parent.mkdir(parents=True)
        choice.write_text(json.dumps({"BinaryDirectory": str(prior)}))
        self.assertEqual(common.previous_version(self.home, installed, None), prior)

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

    def test_owned_update_needs_no_optin_or_backup_and_preserves_config(self):
        s = self.install(); root = self.repo()
        p = common.prepare_repo(s, root, "default")
        config = {"Runtime": {"Workspace": str(root)}, "Launcher": {"Version": 1}}
        p["config"].parent.mkdir(parents=True, exist_ok=True)
        p["config"].write_text(json.dumps(config)); p["config"].chmod(0o600)
        original = p["config"].read_bytes()
        self.args.docker_sandbox_oauth = True
        new = self.install()
        self.assertTrue(new["DockerOAuth"])
        self.assertEqual(p["config"].read_bytes(), original)
        backups = common.settings_path(self.home).parent / "backups"
        self.assertFalse(backups.exists())

    def test_obsolete_audit_flags_do_not_block_or_reach_native(self):
        self.args.policy_audited, self.args.trust_managed_policy = True, False
        settings = self.install()
        for key in ("PolicyAudited", "TrustManagedPolicy", "TrustSource", "CommandHash"):
            self.assertNotIn(key, settings)
        self.assertNotIn("--claude-policy-audited", self.preflight_calls[-1])

    def test_legacy_daily_wrapper_updates_without_optin_and_keeps_history(self):
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
        self.install()
        self.assertNotEqual(command.read_bytes(), old)
        self.assertFalse((common.settings_path(self.home).parent / "backups").exists())
        self.assertEqual({p: (p.read_bytes(), p.stat().st_mtime_ns) for p in before}, before)

    def test_quickstart_pty_launches_without_enter_or_generated_approval(self):
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
        while process.poll() is None and time.monotonic() < deadline:
            if select.select([master], [], [], 0.1)[0]:
                try: output += os.read(master, 4096)
                except OSError: break
        self.assertEqual(process.wait(timeout=10), 0)
        self.assertIn(b"Quick Start", output)
        self.assertIn("Full Access は OS の隔離ではありません。".encode(), output)
        self.assertNotIn("Git公開".encode(), output)
        self.assertNotIn("[Enterで起動]".encode(), output)
        self.assertNotIn(b"# Unreal Agent", output, "detailed README must not display automatically")
        p = common.repo_paths(self.home, root, "read-instructions")
        self.assertFalse(p["config"].exists())
        self.assertFalse(p["socket"].exists())
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
        self.assertNotIn("--bootstrap-claude-trust-source", argv)
        self.assertNotIn("--bootstrap-claude-policy-audited", argv)
        self.assertEqual(argv[-1], "dev")
        self.assertTrue(env["XDG_CONFIG_HOME"].endswith("/native"))
        for mode in ("readme", "workspace_info"):
            setattr(args, mode, True)
            with patch.dict(os.environ, self.env, clear=True), patch.object(daily, "git_root", return_value=root), patch.object(os, "execve") as execute, patch("builtins.input") as prompt, contextlib.redirect_stdout(io.StringIO()):
                common.launch(args, self.package)
            execute.assert_not_called(); prompt.assert_not_called(); setattr(args, mode, False)

    def test_invalid_instance_and_package_io_report_causes(self):
        with self.assertRaises(pilot.PilotError): common.repo_paths(self.home, self.home / "repo", "../old")
        (self.package / "KIT.json").unlink()
        with self.assertRaises(FileNotFoundError): common.package(self.package)

    def configure(self, *, docker=False, external=False, disabled=False, trust=False, disable_claude=False):
        self.args.configure_providers = True
        self.args.docker_sandbox_oauth = docker
        self.args.external_codex = external
        self.args.disable_codex = disabled
        self.args.policy_audited = self.args.trust_managed_policy = trust
        self.args.disable_claude = disable_claude
        self.args.replace_pilot_launcher = True
        return self.install()

    def test_configure_preserves_explicit_routes_and_uses_native_auto_decision(self):
        self.args.configure_providers = True
        settings = self.install()
        self.assertEqual(settings["CodexRoute"], "external")
        self.assertIn("--codex-auto", self.preflight_calls[-1])
        settings=self.configure(docker=True,trust=True)
        self.assertEqual(settings["CodexRoute"],"docker")
        self.assertEqual(settings["ClaudeDecision"],"enabled")
        self.assertIn("--docker-sandbox-oauth",self.preflight_calls[-1])
        self.assertNotIn("--claude-trust-source",self.preflight_calls[-1])
        self.assertNotIn("--claude-policy-audited", self.preflight_calls[-1])
        self.assertNotIn("--model",self.preflight_calls[-1])
        self.assertNotIn("--submit",self.preflight_calls[-1])
        before=common.settings_path(self.home).read_bytes()
        self.args.external_codex=True
        with self.assertRaises(pilot.PilotError): self.install()
        self.assertEqual(common.settings_path(self.home).read_bytes(),before)
        settings=self.configure(external=True,disable_claude=True)
        self.assertFalse(settings["DockerOAuth"])
        self.assertEqual(settings["ProviderStatus"][1]["Reason"],"operator_disabled")
        # Decisions belong to this HOME; an unchanged setup is idempotent.
        kept=(common.settings_path(self.home).read_bytes(),common.settings_path(self.home).stat().st_mtime_ns)
        self.install()
        self.assertEqual((common.settings_path(self.home).read_bytes(),common.settings_path(self.home).stat().st_mtime_ns),kept)

    def test_install_consent_is_not_interactive_audit(self):
        self.args.configure_providers=True
        with patch.object(sys.stdin,"isatty",return_value=True),patch.object(sys.stdout,"isatty",return_value=True),patch("builtins.input",side_effect=AssertionError("provider question forbidden")) as prompt:
            choices=common.provider_choices(self.args,None)
        prompt.assert_not_called()
        with patch.object(common,"provider_choices",return_value=choices): settings=self.install()
        self.assertEqual(settings["CodexRoute"],"external")
        self.assertNotIn("install-consent",self.preflight_calls[-1])
        self.assertNotIn("--claude-policy-audited",self.preflight_calls[-1])

    def test_existing_mono_runtime_and_live_host_are_not_rewritten_by_setup_or_binding(self):
        for initial in ("docker", "claude"):
            with self.subTest(initial=initial):
                fresh=tempfile.TemporaryDirectory(prefix="uf-",dir="/tmp");self.addCleanup(fresh.cleanup)
                self.home=Path(fresh.name);self.env=dict(self.env,HOME=str(self.home))
                self.args=argparse.Namespace(docker_sandbox_oauth=False,policy_audited=False,trust_managed_policy=False,claude_binary="",replace_pilot_launcher=True)
                self.args.configure_providers=False
                self.args.docker_sandbox_oauth=initial=="docker"
                self.args.policy_audited=self.args.trust_managed_policy=initial=="claude"
                self.args.replace_pilot_launcher=True
                old=self.install();root=self.repo("workspace/"+initial)
                paths=common.prepare_repo(old,root,"default")
                paths["config"].parent.mkdir(parents=True,exist_ok=True)
                runtime={"Runtime":{"Workspace":str(root),"Provider":{"provider":"claude-code" if initial=="claude" else "openai-codex"},"ClaudeCode":{"ManagedPolicyMode":"reject"}},"Providers":{"claude-code":{"ClaudeCode":{"ManagedPolicyMode":"reject"}}},"Launcher":{"Version":1,"DiscoverProviders":False,"ClaudeCode":{"ManagedPolicyMode":"trust"}}}
                paths["config"].write_text(json.dumps(runtime));paths["config"].chmod(0o600)
                history=paths["state"] / "sessions/old/history"
                history.parent.mkdir(parents=True);history.write_bytes(b"KEEP_CANONICAL_HISTORY");history.chmod(0o600)
                server=subprocess.Popen([sys.executable,"-c","import socket,sys,time; s=socket.socket(socket.AF_UNIX);s.bind(sys.argv[1]);s.listen();time.sleep(120)",str(paths["socket"])],env=self.env)
                self.addCleanup(lambda p=server: p.poll() is None and (p.terminate(),p.wait(timeout=10)))
                deadline=time.monotonic()+5
                while not paths["socket"].exists() and time.monotonic()<deadline: time.sleep(.01)
                self.assertTrue(paths["socket"].exists())
                before={p:(p.read_bytes(),p.stat().st_mtime_ns) for p in (paths["config"],history)}
                settings=self.configure(docker=True,trust=True)
                self.assertFalse((paths["root"] / "current.json").exists())
                # Configuration changes keep the workspace's instance and files.
                selected=common.select_repo(settings,root,None)
                self.assertEqual(selected["config"],paths["config"])
                self.assertEqual(selected["state"],paths["state"])
                self.assertEqual(selected["socket"],paths["socket"])
                self.assertTrue(selected["config"].exists())
                repeated=common.select_repo(settings,root,None)
                self.assertEqual(repeated,selected)
                self.assertEqual({p:(p.read_bytes(),p.stat().st_mtime_ns) for p in before},before)
                self.assertIsNone(server.poll(),"old fake Host stopped")
                self.assertEqual(common.select_repo(settings,root,"default")["config"],paths["config"])
                self.assertEqual(common.select_repo(settings,root,None,explicit_config=paths["config"])["config"],paths["config"])
                argv=common.command_for(settings,root,selected,"same-session-name")
                self.assertIn("--bootstrap-codex-docker-oauth",argv); self.assertNotIn("--bootstrap-claude-trust-source",argv)
                choice=common.read_workspace_choice(paths["root"] / "current.json",root)
                self.assertEqual(choice["Instance"], "default")
                self.assertNotIn("Previous", choice)
                self.assertNotIn("ConfigHash", choice)

    def test_build_update_keeps_the_instance_and_restarts_its_host(self):
        settings=self.configure(external=True,trust=True);root=self.repo()
        one=common.select_repo(settings,root,None)
        route=settings.get("CodexRoute","docker" if settings["DockerOAuth"] else "external")
        config={"Launcher":{"Version":1,"CodexDockerSandboxOAuth":settings["DockerOAuth"],"CodexAuto":route=="auto","CodexDisabled":route=="disabled",
                            "ClaudeDisabled":settings.get("ClaudeDecision")=="disabled","ClaudeBinary":settings["ClaudeBinary"]},
                "Runtime":{"Workspace":str(root),"Provider":{}},"Providers":{}}
        one["config"].parent.mkdir(parents=True,exist_ok=True)
        one["config"].write_text(json.dumps(config));one["config"].chmod(0o600)
        self.assertEqual(common.select_repo(settings,root,None),one)
        stopped=[]
        original=common.stop_old_host;common.stop_old_host=lambda paths: stopped.append(paths["config"])
        try:
            updated=dict(settings,Revision="f"*40)
            self.assertEqual(common.select_repo(updated,root,None),one)
        finally:
            common.stop_old_host=original
        self.assertEqual(stopped,[one["config"]])
        choice=common.read_workspace_choice(one["root"] / "current.json",root)
        self.assertEqual(choice["Revision"],"f"*40)

    def test_workspace_profile_fresh_idempotent_and_availability_not_identity(self):
        settings=self.configure(external=True,trust=True);root=self.repo()
        one=common.select_repo(settings,root,None)
        choice=one["root"] / "current.json";before=(choice.read_bytes(),choice.stat().st_mtime_ns)
        changed=dict(settings,ProviderStatus=[{"ID":"openai-codex","Availability":"unavailable","Reason":"auth_required"},{"ID":"claude-code","Availability":"available","Reason":""}])
        two=common.select_repo(changed,root,None)
        self.assertEqual(one,two)
        self.assertEqual((choice.read_bytes(),choice.stat().st_mtime_ns),before)
        # Unrelated sibling configuration paths and redirected choice are rejected.
        other=self.home / "operator.json";other.write_text(json.dumps({"Runtime":{"Workspace":str(root)}}));other.chmod(0o600)
        self.assertTrue(common.select_repo(settings,root,None,explicit_config=other)["explicit_config"])
        wrong=self.home / "wrong.json";wrong.write_text('{}');wrong.chmod(0o600)
        with self.assertRaises(pilot.PilotError): common.select_repo(settings,root,None,explicit_config=wrong)

    def test_preflight_status_accepts_the_claude_code_route(self):
        # The Claude Code route (U21) reports as "claude"; an older binary's
        # "claude-code" stays readable, and anything else is refused.
        for claude in ("claude", "claude-code"):
            status=[{"ID":"openai-codex","Availability":"available","Reason":""},{"ID":claude,"Availability":"available","Reason":""}]
            self.assertEqual(common.validate_status(status),status)
        with self.assertRaises(pilot.PilotError):
            common.validate_status([{"ID":"openai-codex","Availability":"available","Reason":""},{"ID":"claude-native","Availability":"available","Reason":""}])

    def test_provider_profile_ignores_retired_audit_settings(self):
        settings = self.configure(external=True, trust=True)
        changed = dict(settings, PolicyAudited=True, TrustManagedPolicy=False, TrustSource="old")
        self.assertEqual(common.provider_profile(changed), common.provider_profile(settings))
        self.assertNotEqual(common.provider_profile(dict(settings, ClaudeBinary="/opt/claude")), common.provider_profile(settings))

    def test_changed_native_config_keeps_the_same_instance(self):
        settings=self.configure(external=True,trust=True);root=self.repo()
        paths=common.select_repo(settings,root,None)
        paths["config"].parent.mkdir(parents=True,exist_ok=True)
        config={"Runtime":{"Workspace":str(root),"Provider":{"provider":"claude-code"},
                    "ClaudeCode":{"managedPolicyMode":"trust","ToolBridge":{"Enabled":True,"Mode":"structured"}}},
                "Launcher":{"Version":1}}
        paths["config"].write_text(json.dumps(config));paths["config"].chmod(0o600)
        self.assertEqual(common.select_repo(settings,root,None),paths)
        choice=paths["root"] / "current.json"
        before=(choice.read_bytes(),choice.stat().st_mtime_ns)
        self.assertEqual(common.select_repo(settings,root,None),paths)
        self.assertEqual((choice.read_bytes(),choice.stat().st_mtime_ns),before)
        config["Runtime"]["ClaudeCode"]["managedPolicyMode"]="reject"
        paths["config"].write_text(json.dumps(config))
        old=(paths["config"].read_bytes(),paths["config"].stat().st_mtime_ns)
        changed=common.select_repo(settings,root,None)
        self.assertEqual(changed["config"],paths["config"])
        self.assertEqual(common.read_workspace_choice(choice,root)["Instance"],"default")
        self.assertEqual((paths["config"].read_bytes(),paths["config"].stat().st_mtime_ns),old)

    def test_legacy_settings_and_workspace_choice_remain_readable(self):
        saved = self.install()
        root = self.repo()
        paths = common.prepare_repo(saved, root, "p-existing")
        paths["config"].parent.mkdir(parents=True, exist_ok=True)
        config = {"Runtime": {"Workspace": str(root)}, "Launcher": {"Version": 1}}
        paths["config"].write_text(json.dumps(config)); paths["config"].chmod(0o600)
        default = common.prepare_repo(saved, root, "default")
        current = default["root"] / "current.json"
        old_choice = {"Version": 1, "Workspace": str(root), "Instance": "p-existing",
                      "Profile": "a"*64, "ConfigHash": "b"*64, "Previous": [{}]*40,
                      "Revision": saved["Revision"], "BinaryDirectory": saved["BinaryDirectory"]}
        current.write_text(json.dumps(old_choice)); current.chmod(0o600)
        for version in (1, 2, 3):
            old = dict(saved, Version=version, PolicyAudited=False, TrustManagedPolicy=True,
                       TrustSource="legacy-value", CommandHash="outdated")
            common.settings_path(self.home).write_text(json.dumps(old))
            restored = common.read_settings(self.home)
            self.assertEqual(restored["Version"], version)
            with patch.object(common, "stop_old_host"):
                self.assertEqual(common.select_repo(restored, root, None), paths)
            self.assertEqual(json.loads(paths["config"].read_bytes()), config)
        # Reformatting runtime.json never selects another history directory.
        paths["config"].write_text(json.dumps(config, indent=4))
        self.assertEqual(common.select_repo(saved, root, None), paths)

    def test_private_settings_redirect_remains_protected(self):
        self.install()
        path = common.settings_path(self.home)
        target = self.home / "copied-binding"
        target.write_bytes(path.read_bytes()); target.chmod(0o600)
        path.unlink(); path.symlink_to(target)
        with self.assertRaises(OSError): common.read_settings(self.home)

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


class InstallGuidanceTests(unittest.TestCase):
    def test_four_login_states(self):
        for codex, claude in ((True, False), (False, True), (False, False), (True, True)):
            with self.subTest(codex=codex, claude=claude):
                statuses = [{"ID": provider, "Availability": "available" if ready else "unavailable", "Reason": "" if ready else "auth_required"}
                            for provider, ready in (("openai-codex", codex), ("claude", claude))]
                output = io.StringIO()
                with patch.object(common, "shell_masks_unreal", return_value=False), contextlib.redirect_stdout(output):
                    common.print_install_result({"ProviderStatus": statuses}, Path("/tmp/synthetic-home"))
                text = output.getvalue()
                self.assertTrue(text.startswith("インストール完了\n"))
                self.assertIn("Codex: " + ("利用できます" if codex else "ログインが必要です"), text)
                self.assertIn("Claude: " + ("利用できます" if claude else "ログインが必要です"), text)
                self.assertEqual("codex login" in text, not codex)
                self.assertEqual("claude を起動して /login" in text, not claude)
                self.assertEqual("使うにはどちらかの準備" in text, not codex and not claude)
                self.assertEqual("Claude の操作は" in text, claude)
                for hidden in ("auth_required", "unavailable", "aliases/functions", "NOTE:"):
                    self.assertNotIn(hidden, text)

    def test_missing_clis_and_actual_shadow_only(self):
        settings = {"ProviderStatus": [{"ID": p, "Availability": "unavailable", "Reason": "cli_missing_or_unsupported"}
                                      for p in ("openai-codex", "claude")]}
        for masked in (True, False):
            output = io.StringIO()
            with patch.object(common, "shell_masks_unreal", return_value=masked), contextlib.redirect_stdout(output):
                common.print_install_result(settings, Path("/tmp/synthetic-home"))
            text = output.getvalue()
            for word in ("をインストールしてから", "Codex CLI", "Claude Code", "unreal を起動"):
                self.assertIn(word, text)
            self.assertEqual("type -a unreal" in text, masked)
        with patch.object(pilot, "bounded_probe", return_value=(b"UNREAL_MASKED:no\n", 0)) as probe:
            self.assertFalse(common.shell_masks_unreal(Path("/tmp/synthetic-home")))
            self.assertIn("type -a unreal", probe.call_args.args[0][-3])
        with patch.object(pilot, "bounded_probe", return_value=(b"UNREAL_MASKED:yes\n", 0)):
            self.assertTrue(common.shell_masks_unreal(Path("/tmp/synthetic-home")))

    def test_kit_readme_matches_current_updates(self):
        text = (Path(__file__).parent / "pilot/common-README.md").read_text()
        for word in ("どちらか一方", "codex login", "claude", "/login", "再起動", "会話と設定を引き継"):
            self.assertIn(word, text)
        for old in ("別instanceで新規Session", "wait`", "--timeout 60s", "子に渡したらターンを終え"):
            self.assertNotIn(old, text)

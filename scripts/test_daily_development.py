import argparse
import contextlib
import ctypes
import fcntl
import hashlib
import io
import json
import os
from pathlib import Path
import pty
import select
import shutil
import signal
import struct
import subprocess
import sys
import tarfile
import tempfile
import time
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).parent))
import daily_development as daily
import daily_development_bundle as bundle
import devlore_launcher_bundle as legacy_bundle
import pilot_kit
import pilot_provider as pilot
import permission_acceptance as gate
from test_pilot_kit import source_archive


class DailyDevelopmentTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="dd-", dir="/tmp")
        self.addCleanup(self.temp.cleanup)
        self.home = Path(self.temp.name)
        self.env = {"HOME": str(self.home), "PATH": "/usr/bin:/bin", "PYTHONDONTWRITEBYTECODE": "1"}
        output = self.home / "output"; output.mkdir()
        source = source_archive(self.home, revision="a" * 40)
        with patch.object(pilot_kit, "binary_requirements", return_value="2.34"):
            self.archive = bundle.assemble(source, output, "a" * 40, "asp")
        self.package = output / "unreal-agent-asp-development-aaaaaaa"
        self.meta = json.loads((self.package / "KIT.json").read_bytes())
        self.args = argparse.Namespace(model=None, effort=None, claude_binary="claude", policy_audited=False,
                                       trust_managed_policy=False, replace_pilot_launcher=False)
        self.launch_args = argparse.Namespace(session="development", readme=False, workspace_info=False)
        self.original_probe = pilot.bounded_probe

    def probe(self, argv, env, **kwargs):
        if argv[:2] == ["bash", str(self.package / "install.sh")]:
            identity = hashlib.sha256((self.package / "SHA256SUMS").read_bytes()).hexdigest()[:16]
            installed = self.home / ".local/opt/unreal-agent-pilot" / self.meta["sandbox"] / (self.meta["baseline"][:7] + "-" + identity)
            if not installed.exists():
                shutil.copytree(self.package, installed)
            return b"", 0
        if argv[0] == "bash" and argv[1].endswith("doctor.sh"):
            return b"", 0
        if argv[0] in ("git", "/bin/bash"):
            return self.original_probe(argv, env, **kwargs)
        raise AssertionError("unexpected subprocess; no model inference allowed")

    def install(self):
        with patch.dict(os.environ, self.env, clear=True), patch.object(pilot, "bounded_probe", side_effect=self.probe), \
                patch.object(daily, "preflight", return_value=("/existing/auth.json", "") if self.meta["sandbox"] == "asp" else ("", "/existing/claude")), \
                contextlib.redirect_stdout(io.StringIO()):
            daily.install(self.args, self.package, self.meta)
        return daily.read_settings(self.home, self.meta["sandbox"])

    def repo(self, name="workspace/project"):
        path = self.home / name; path.mkdir(parents=True, exist_ok=True)
        subprocess.run(["git", "init", "--quiet", str(path)], check=True, env=self.env,
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        return path

    def root(self, path, env=None):
        with patch.dict(os.environ, env or self.env, clear=True), patch.object(Path, "cwd", return_value=path):
            return daily.git_root()

    def test_git_root_subdirectory_spaces_unicode_and_symlink_alias(self):
        root = self.repo("workspace/日本語 project")
        sub = root / "dir"; sub.mkdir()
        alias = self.home / "alias"; alias.symlink_to(root, target_is_directory=True)
        before = subprocess.check_output(["git", "-C", str(root), "status", "--porcelain"])
        for directory in (root, sub, alias, alias / "dir"):
            self.assertEqual(self.root(directory), root)
        self.assertEqual(subprocess.check_output(["git", "-C", str(root), "status", "--porcelain"]), before)
        self.assertFalse((self.home / ".config").exists())

    def test_git_worktrees_and_equal_session_names_are_distinct(self):
        root = self.repo()
        (root / "keep.txt").write_text("fixture\n")
        subprocess.run(["git", "-C", str(root), "add", "keep.txt"], check=True, env=self.env)
        subprocess.run(["git", "-C", str(root), "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid",
                        "commit", "--quiet", "-m", "fixture"], check=True, env=self.env)
        worktree = self.home / "workspace/another"
        subprocess.run(["git", "-C", str(root), "worktree", "add", "--quiet", "--detach", str(worktree)], check=True, env=self.env)
        roots = [self.root(root), self.root(worktree), self.root(self.repo("workspace/third"))]
        paths = [daily.repo_paths(self.home, "asp", r) for r in roots]
        self.assertEqual(len({p["socket"] for p in paths}), 3)
        self.assertEqual(len({p["sessions"] for p in paths}), 3)
        self.assertNotEqual(paths[0]["socket"], daily.repo_paths(self.home, "devlore", root)["socket"])

    def test_nonrepo_bare_overrides_and_private_storage_rejected(self):
        nonrepo = self.home / "workspace"; nonrepo.mkdir()
        bare = self.home / "bare"; subprocess.run(["git", "init", "--bare", "--quiet", str(bare)], check=True, env=self.env)
        for path in (nonrepo, bare):
            with self.assertRaises(pilot.PilotError) as failure: self.root(path)
            self.assertEqual(failure.exception.code, "development_git_repository_required")
        root = self.repo()
        for key in ("GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE"):
            with self.assertRaises(pilot.PilotError) as failure: self.root(root, dict(self.env, **{key: "PRIVATE_VALUE"}))
            self.assertEqual(failure.exception.code, "development_git_override")
            self.assertNotIn("PRIVATE_VALUE", str(failure.exception))
        for name in (".config/project", ".codex/project", ".local/state/project"):
            with self.assertRaises(pilot.PilotError) as failure: self.root(self.repo(name))
            self.assertEqual(failure.exception.code, "development_workspace_private")

    def test_unsafe_paths_and_socket_length_fail_closed(self):
        for value in ("relative", "/tmp/line\nbreak", "/tmp/escape\x1b[31m", "/tmp/bidi\u202e", "/tmp/null\0"):
            with self.assertRaises(pilot.PilotError): daily.safe_path(value)
        with self.assertRaises(pilot.PilotError): daily.repo_paths(Path("/" + "x" * 100), "asp", self.home)
        self.assertEqual(daily.safe_path("/tmp/日本語 path"), Path("/tmp/日本語 path"))

    def test_install_fresh_bash_idempotency_and_protected_settings(self):
        keep = []
        for name in (".codex/auth.json", ".claude/credentials", ".config/unreal-agent/runtime.json",
                     ".local/state/unreal-agent/sessions/existing", ".local/state/unreal-agent-pilot/asp/permissions-old/sessions/gate-e",
                     ".local/state/unreal-agent-pilot/devlore/launcher-v1/sessions/default"):
            path = self.home / name; path.parent.mkdir(parents=True, exist_ok=True); path.write_bytes(b"PRIVATE_KEEP"); keep.append(path)
        initial = {p: (p.read_bytes(), p.stat().st_mtime_ns) for p in keep}
        self.install()
        self.assertFalse((self.home / "workspace").exists(), "install must not assume/create a workspace")
        self.assertFalse((self.home / ".local/state/unreal-agent-dev").exists())
        command = self.home / ".local/bin/unreal"
        for flags in (("--noprofile", "-ic"), ("-lic",)):
            result = subprocess.run(["bash", *flags, 'found=$(command -v unreal) && printf "\\nCOMMAND_PATH=%s\\n" "$found"'],
                                    env=self.env, capture_output=True, text=True, timeout=15)
            self.assertEqual(result.returncode, 0)
            self.assertIn("COMMAND_PATH=" + str(command), result.stdout)
        kept = [*keep, command, daily.settings_path(self.home, "asp"), self.home / ".bashrc", self.home / ".profile"]
        before = {p: (p.read_bytes(), p.stat().st_mtime_ns) for p in kept}
        self.install()
        self.assertEqual({p: (p.read_bytes(), p.stat().st_mtime_ns) for p in kept}, before)
        self.assertEqual({p: (p.read_bytes(), p.stat().st_mtime_ns) for p in keep}, initial)
        self.assertEqual(command.stat().st_mode & 0o777, 0o700)
        self.assertEqual(daily.settings_path(self.home, "asp").stat().st_mode & 0o777, 0o600)

    def test_existing_arbitrary_command_alias_function_symlink_not_replaced(self):
        command = self.home / ".local/bin/unreal"; command.parent.mkdir(parents=True)
        for kind in ("file", "alias", "function", "symlink"):
            with self.subTest(kind=kind):
                rc = self.home / ".bashrc"
                if kind == "file": command.write_bytes(b"PRIVATE_EXISTING_COMMAND"); command.chmod(0o700)
                elif kind == "symlink": command.symlink_to(self.package / "unreal")
                else: rc.write_text("alias unreal='echo PRIVATE_VALUE'\n" if kind == "alias" else "unreal() { echo PRIVATE_VALUE; }\n")
                self.args.replace_pilot_launcher = True
                with self.assertRaises((pilot.PilotError, FileNotFoundError)): self.install()
                self.assertFalse(daily.settings_path(self.home, "asp").exists())
                if kind == "file": self.assertEqual(command.read_bytes(), b"PRIVATE_EXISTING_COMMAND")
                if os.path.lexists(command): command.unlink()  # synthetic test fixture only
                if rc.exists(): rc.unlink()

    def test_owned_update_requires_optin_and_preserves_repo_startup_config(self):
        settings = self.install()
        root = self.repo()
        p = daily.prepare_repo(settings, root)
        cfg = pilot.decode(p["config"].read_bytes())
        cfg["PermissionModes"]["Maximum"]["Tools"].remove("edit")
        pilot.publish_json(p["config"], cfg, replace=True)
        before = p["config"].read_bytes(), p["config"].stat().st_mtime_ns
        self.args.model = "different-model"
        with self.assertRaises(pilot.PilotError): self.install()
        self.args.replace_pilot_launcher = True
        new = self.install()
        self.assertEqual(new["Model"], "different-model")
        self.assertTrue(list(daily.settings_path(self.home, "asp").parent.joinpath("backups").glob("*.json")))
        self.assertEqual(daily.prepare_repo(new, root), p)
        self.assertEqual((p["config"].read_bytes(), p["config"].stat().st_mtime_ns), before)
        fresh = daily.prepare_repo(new, self.repo("workspace/new"))
        self.assertEqual(pilot.decode(fresh["config"].read_bytes())["Runtime"]["Provider"]["model"]["id"], "different-model")

    def test_legacy_devlore_exact_wrapper_optin_without_migration(self):
        self.meta["sandbox"] = "devlore"
        (self.package / "KIT.json").write_text(json.dumps(self.meta)); (self.package / "KIT.json").chmod(0o600)
        (self.package / "SHA256SUMS").write_text("".join(hashlib.sha256((self.package / name).read_bytes()).hexdigest() + "  " + name + "\n" for name in daily.MEMBERS))
        self.args.policy_audited = self.args.trust_managed_policy = True
        directory = self.home / ".local/opt/unreal-agent-pilot/devlore"; directory.mkdir(parents=True)
        output = self.home / "legacy"; output.mkdir()
        legacy_bundle.build(output, "c" * 40)
        old_pkg = output / "unreal-agent-devlore-launcher-ccccccc"
        identity = hashlib.sha256((old_pkg / "SHA256SUMS").read_bytes()).hexdigest()[:16]
        old_code = directory / ("launcher-v1-" + identity); shutil.copytree(old_pkg, old_code)
        bdir = self.home / ".config/unreal-agent-pilot/devlore/launcher-v1"; bdir.mkdir(parents=True, mode=0o700)
        pilot.publish_json(bdir / "launcher.json", {"Version": 1, "Home": str(self.home), "BinaryDirectory": "/old/native",
                           "BinaryRevision": pilot.BASELINE, "PolicyAudited": True})
        command = self.home / ".local/bin/unreal"; command.parent.mkdir(parents=True)
        import shlex
        original = ("#!/bin/bash\n# Unreal Agent devlore permanent launcher; no shell evaluation.\nexec "
                    + shlex.quote(str(Path(sys.executable).resolve())) + " " + shlex.quote(str(old_code / "devlore_launcher.py"))
                    + ' launch "$@"\n').encode()
        command.write_bytes(original); command.chmod(0o700)
        before = (bdir / "launcher.json").read_bytes()
        with self.assertRaises(pilot.PilotError): self.install()
        self.args.replace_pilot_launcher = True
        self.install()
        self.assertNotEqual(command.read_bytes(), original)
        self.assertEqual((bdir / "launcher.json").read_bytes(), before)
        backups = daily.settings_path(self.home, "devlore").parent / "backups"
        self.assertEqual(next(backups.glob("*.sh")).read_bytes(), original)

    def test_auth_missing_and_managed_audit_required_no_installation(self):
        with patch.dict(os.environ, self.env, clear=True), patch.object(daily, "preflight", side_effect=pilot.PilotError("auth_missing", "No fallback.")), self.assertRaises(pilot.PilotError):
            daily.install(self.args, self.package, self.meta)
        self.assertFalse(daily.settings_path(self.home, "asp").exists())
        self.meta["sandbox"] = "devlore"
        with patch.dict(os.environ, self.env, clear=True), patch.object(daily, "preflight", side_effect=AssertionError("must not probe")), self.assertRaises(pilot.PilotError) as failure:
            daily.install(self.args, self.package, self.meta)
        self.assertEqual(failure.exception.code, "managed_policy_audit_required")
        self.assertFalse((self.home / ".local/bin/unreal").exists())

    def test_checksum_provenance_and_unknown_package_members(self):
        with patch.dict(os.environ, self.env, clear=True), patch.object(pilot, "bounded_probe", side_effect=self.probe):
            self.assertEqual(daily.package(self.package), (self.package, self.meta))
            (self.package / "daily_development.py").write_bytes(b"PRIVATE_TAMPERED")
            with self.assertRaises(pilot.PilotError) as failure: daily.package(self.package)
            self.assertEqual(failure.exception.code, "development_kit_checksum")
            self.assertNotIn("PRIVATE_TAMPERED", str(failure.exception))

    def test_permission_defaults_and_provider_contracts(self):
        for sandbox in ("asp", "devlore"):
            cfg = daily.configuration(sandbox, self.home / "project", "explicit-model", "high", "/existing/claude",
                                      audited=sandbox == "devlore", trust=sandbox == "devlore")
            self.assertEqual(cfg["Permissions"]["Tools"], ["read", "grep", "glob"])
            self.assertEqual(cfg["Permissions"]["ReadRoots"], [str(self.home / "project")])
            self.assertEqual(cfg["Permissions"]["WriteRoots"], [])
            self.assertEqual(cfg["Permissions"]["ProcessMode"], "")
            self.assertFalse(cfg["Permissions"]["FilesystemUnrestricted"])
            self.assertFalse(cfg["Permissions"]["NetworkUnrestricted"])
            self.assertTrue(cfg["PermissionModes"]["Enabled"])
            self.assertNotIn("Mode", cfg["PermissionModes"])
            self.assertEqual(cfg["Runtime"]["Provider"]["max_attempts"], 1)
            self.assertFalse(cfg["Launcher"]["DiscoverProviders"])
            if sandbox == "asp": self.assertEqual(cfg["Runtime"]["Provider"]["auth"]["method"], "docker_sandbox_oauth")
            else:
                self.assertEqual(cfg["Runtime"]["ClaudeCode"]["managedPolicyMode"], "trust")
                self.assertEqual(cfg["Runtime"]["ClaudeCode"]["ToolBridge"]["Mode"], "structured")
                self.assertEqual(cfg["Runtime"]["ClaudeCode"]["ToolBridge"]["MaxActionRounds"], 32)

    def test_repo_identity_collision_config_redirect_and_runtime_mismatch_rejected(self):
        settings = self.install()
        p = daily.prepare_repo(settings, self.repo())
        binding = pilot.decode(p["binding"].read_bytes()); binding["Identity"] = "b" * 64
        pilot.publish_json(p["binding"], binding, replace=True)
        before = p["config"].read_bytes(), p["binding"].read_bytes()
        with self.assertRaises(pilot.PilotError): daily.prepare_repo(settings, p["workspace"])
        self.assertEqual((p["config"].read_bytes(), p["binding"].read_bytes()), before)
        other = daily.prepare_repo(settings, self.repo("workspace/other"))
        contents = other["config"].read_bytes(); other["config"].unlink(); other["config"].symlink_to(p["config"])
        with self.assertRaises((pilot.PilotError, OSError)): daily.prepare_repo(settings, other["workspace"])
        self.assertEqual(p["config"].read_bytes(), before[0])
        other["config"].unlink(); other["config"].write_bytes(contents); other["config"].chmod(0o600)
        cfg = pilot.decode(contents); cfg["Runtime"]["Workspace"] = "/other/repo"
        pilot.publish_json(other["config"], cfg, replace=True)
        with self.assertRaises(pilot.PilotError): daily.prepare_repo(settings, other["workspace"])

    def test_native_launcher_arguments_workspace_and_no_auto_submission(self):
        settings = self.install(); root = self.repo(); sub = root / "sub"; sub.mkdir()
        old_cwd = os.getcwd(); self.addCleanup(os.chdir, old_cwd)
        with patch.dict(os.environ, self.env, clear=True), patch.object(Path, "cwd", return_value=sub), \
                patch.object(daily, "preflight", return_value=(settings["CodexFile"], "")), \
                patch.object(os, "execv") as execute, patch.object(pilot, "smoke", side_effect=AssertionError("no inference")), \
                patch.object(gate, "instructions") as instructions:
            daily.launch(self.launch_args, Path(settings["BinaryDirectory"]), self.meta)
            command = execute.call_args.args[1]
            self.assertEqual(command[0], str(Path(settings["BinaryDirectory"]) / "unreal"))
            self.assertEqual(command[-1], "development")
            self.assertEqual(os.getcwd(), str(root))
            self.assertIn(str(daily.repo_paths(self.home, "asp", root)["socket"]), command)
            self.assertIn("--codex-auth-file", command)
            instructions.assert_called_once_with(Path(settings["BinaryDirectory"]), wait=True, readme_command="unreal --readme")
            for bad in ("bad_name", "bad.name", "-flag", "", "x" * 129):
                self.launch_args.session = bad
                with self.assertRaises(pilot.PilotError): daily.launch(self.launch_args, Path(settings["BinaryDirectory"]), self.meta)
            self.assertEqual(execute.call_count, 1)

    def test_workspace_info_and_readme_no_exec_wait_or_inference(self):
        settings = self.install(); root = self.repo()
        self.launch_args.workspace_info = True
        output = io.StringIO()
        with patch.dict(os.environ, self.env, clear=True), patch.object(Path, "cwd", return_value=root), \
                patch.object(daily, "preflight", return_value=(settings["CodexFile"], "")), patch.object(os, "execv") as execute, \
                patch.object(gate, "instructions", side_effect=AssertionError("must not wait")), contextlib.redirect_stdout(output):
            daily.launch(self.launch_args, Path(settings["BinaryDirectory"]), self.meta)
        self.assertEqual(json.loads(output.getvalue())["workspace"], str(root))
        execute.assert_not_called()
        self.launch_args.readme = True
        with patch.object(daily, "git_root", side_effect=AssertionError("readme needs no repo")), patch.object(daily, "preflight", side_effect=AssertionError("readme needs no auth")), contextlib.redirect_stdout(io.StringIO()):
            daily.launch(self.launch_args, self.package, self.meta)

    def test_archive_inventory_checksums_and_no_credentials(self):
        with tarfile.open(self.archive) as archive:
            self.assertEqual({Path(m.name).name for m in archive}, set(daily.MEMBERS) | {"SHA256SUMS"})
            self.assertTrue(all(m.isfile() for m in archive))
            text = archive.extractfile(self.package.name + "/README.md").read().decode()
            self.assertIn("unreal --workspace-info", text)
            self.assertIn("unreal SESSION_NAME", text)
            self.assertNotIn("@MODEL@", text)
        subprocess.run(["bash", "-n", str(self.package / "daily.sh")], check=True)

    def test_provider_preflight_reuses_exact_native_routes_and_no_secret_values(self):
        with patch.dict(os.environ, self.env, clear=True), patch.object(daily.asp, "preflight", return_value="/existing/auth") as codex, \
                patch.object(pilot, "claude_preflight", return_value="/existing/claude") as claude:
            self.assertEqual(daily.preflight(self.package, self.home, "asp"), ("/existing/auth", ""))
            codex.assert_called_once_with(self.package, self.home, os.environ)
            claude.assert_not_called()
            self.assertEqual(daily.preflight(self.package, self.home, "devlore", binary="/existing/claude", audited=True, trust=True), ("", "/existing/claude"))
            claude.assert_called_once_with("/existing/claude", self.home, os.environ, True, True)


@unittest.skipUnless(os.environ.get("UNREAL_DAILY_KIT_DIRECTORY"), "set UNREAL_DAILY_KIT_DIRECTORY for offline packaged PTY/Host checks")
class DailyPackagedTests(unittest.TestCase):
    """Real installer/native TUI, isolated disposable repositories, fake CLI only."""

    def test_new_bash_repository_hosts_detach_resume_catalog_and_permission_expiry(self):
        original_package = Path(os.environ["UNREAL_DAILY_KIT_DIRECTORY"])
        meta = json.loads((original_package / "KIT.json").read_bytes())
        with tempfile.TemporaryDirectory(prefix="dp-", dir="/tmp") as temporary:
            user_home = Path(temporary)
            env = {"HOME": str(user_home), "PATH": "/usr/bin:/bin", "TERM": "xterm-256color", "LANG": "C.UTF-8",
                   "NO_COLOR": "1", "PYTHONDONTWRITEBYTECODE": "1", "SBX_CRED_OPENAI_MODE": "oauth",
                   "HTTPS_PROXY": "http://host.docker.internal:3128"}
            codex = user_home / ".codex"; codex.mkdir()
            for name, content in {
                "auth.json": '{"OPENAI_API_KEY":"proxy-managed"}',
                "config.toml": 'forced_login_method="api"\nmodel_provider="sandboxd"\n[model_providers.sandboxd]\nname="Sandbox Proxy"\nbase_url="https://chatgpt.com/backend-api/codex"\nexperimental_bearer_token="oai-oat01-proxy-managed"\nrequires_openai_auth=false\n',
                "models_cache.json": json.dumps({"models": [{"slug": "gpt-6.1-sol", "display_name": "Offline Codex", "visibility": "list",
                    "default_reasoning_level": "high", "supported_reasoning_levels": [{"effort": "high"}, {"effort": "medium"}]}]}),
            }.items():
                (codex / name).write_text(content); (codex / name).chmod(0o600)
            keep = user_home / ".local/state/unreal-agent-pilot" / meta["sandbox"] / "permissions-old/sessions/gate-e"
            keep.parent.mkdir(parents=True); keep.write_bytes(b"old gate history remains")
            before = {p: (p.read_bytes(), p.stat().st_mtime_ns) for p in (*codex.iterdir(), keep)}
            options = []
            if meta["sandbox"] == "devlore":
                fake = os.environ.get("UNREAL_DAILY_FAKE_CLAUDE", "")
                self.assertTrue(fake, "Explicit offline fake binary required; never discover real Claude")
                fake_dir = user_home / "fake"; fake_dir.mkdir()
                fake_binary = fake_dir / "claude"; shutil.copyfile(fake, fake_binary); fake_binary.chmod(0o700)
                fake_config = {"Subscription": "team", "Catalog": json.dumps([{"value": "opus", "resolvedModel": "offline-opus",
                    "displayName": "Offline Opus", "supportsEffort": True, "supportedEffortLevels": ["high", "low"]}]),
                    "StructuredResponses": [json.dumps({"type": "action", "action": {"id": "offline-read", "tool": "read", "arguments": {"path": "source.txt"}}}),
                                            json.dumps({"type": "final", "final": {"message": "DAILY_FAKE_OK"}})]}
                (fake_dir / "fake.json").write_text(json.dumps(fake_config))
                options = ["--claude-binary", str(fake_binary), "--policy-audited", "--trust-managed-policy"]
            install_args = ["bash", str(original_package / "daily.sh"), "install", *options]
            for _ in range(2):
                result = subprocess.run(install_args, cwd=user_home, env=env, capture_output=True, timeout=120)
                self.assertEqual(result.returncode, 0, "packaged install failed; details withheld: " + str(result.stderr[:256]))
            settings = daily.read_settings(user_home, meta["sandbox"])
            installed = Path(settings["BinaryDirectory"])
            repos = []
            for name in ("workspace/first repo", "workspace/second repo"):
                root = user_home / name; root.mkdir(parents=True)
                subprocess.run(["git", "init", "--quiet", str(root)], env=env, check=True)
                (root / "source.txt").write_text("DAILY_SOURCE_MARKER\n")
                repos.append(root)
            for flags in (("--noprofile", "-ic"), ("-lic",)):
                result = subprocess.run(["bash", *flags, 'found=$(command -v unreal) && printf "\\nFOUND=%s\\n" "$found"'], env=env, capture_output=True, text=True, timeout=20)
                self.assertEqual(result.returncode, 0)
                self.assertIn("FOUND=" + str(user_home / ".local/bin/unreal"), result.stdout)

            libc = ctypes.CDLL(None, use_errno=True)
            old_subreaper = ctypes.c_int()
            self.assertEqual(libc.prctl(37, ctypes.byref(old_subreaper), 0, 0, 0), 0)
            self.assertEqual(libc.prctl(36, 1, 0, 0, 0), 0)
            owners = {}
            def stop_owner(p):
                pid = owners.pop(str(p["socket"]), 0)
                if not pid: return
                gate.validate_pid(pid, installed, p, {"CodexFile": settings["CodexFile"]})
                os.kill(pid, signal.SIGTERM)
                deadline = time.monotonic() + 20
                while time.monotonic() < deadline:
                    found, status = os.waitpid(pid, os.WNOHANG)
                    if found:
                        self.assertEqual(os.waitstatus_to_exitcode(status), 0)
                        return
                    time.sleep(.02)
                self.fail("verified dedicated Host did not stop")

            def inspect(p, name):
                view = pilot.Gateway(p["socket"]).call("inspect", {"ID": name, "Limit": 256})["View"]
                self.assertFalse(view["History"].get("More"), "fresh fixture history must fit the gateway page")
                return view

            def terminal(root, name, *, generate=False, full=False, expected_mode="read_only"):
                master, slave = pty.openpty()
                fcntl.ioctl(slave, 0x5414, struct.pack("HHHH", 30, 130, 0, 0))
                text = "unreal" + (" " + name if name else "")
                child = subprocess.Popen(["bash", "--noprofile", "-ic", text], env=env, cwd=root,
                    stdin=slave, stdout=slave, stderr=slave, start_new_session=True)
                os.close(slave)
                screen = bytearray()
                def until(marker):
                    marker = marker.encode(); deadline = time.monotonic() + 30
                    while time.monotonic() < deadline:
                        if marker in screen: return
                        ready, _, _ = select.select([master], [], [], max(0, deadline - time.monotonic()))
                        if ready:
                            try: screen.extend(os.read(master, 65536))
                            except OSError: self.fail("PTY ended before expected screen; payload withheld")
                    self.fail("PTY missing expected screen: " + marker.decode())
                def send(value):
                    screen.clear(); os.write(master, value)
                p = daily.repo_paths(user_home, meta["sandbox"], root)
                try:
                    until("[Enterで起動]")
                    self.assertFalse(p["socket"].exists() and str(p["socket"]) not in owners, "Quick Start must precede new Host")
                    send(b"\n")
                    until("connected")
                    pid = gate.peer_pid(p["socket"])
                    gate.validate_pid(pid, installed, p, {"CodexFile": settings["CodexFile"]})
                    if str(p["socket"]) in owners: self.assertEqual(pid, owners[str(p["socket"])], "duplicate Host")
                    owners[str(p["socket"])] = pid
                    sid = name or "default"
                    view = inspect(p, sid)
                    self.assertEqual(view["Permissions"]["Mode"], expected_mode)
                    cfg = pilot.decode(p["config"].read_bytes())
                    self.assertEqual(cfg["Runtime"]["Workspace"], str(root))
                    catalog = pilot.Gateway(p["socket"]).call("extension", {"Action": "model.catalog", "ID": sid})["Catalog"]
                    self.assertTrue(catalog["Available"])
                    send(b"/model\r"); until("Model")
                    send(b"\x1b")
                    until("connected")
                    if generate:
                        self.assertEqual(meta["sandbox"], "devlore", "asp test must not contact real proxy")
                        send(b"read synthetic source\r"); until("DAILY_FAKE_OK")
                        view = inspect(p, sid)
                        self.assertFalse(view.get("Failure"))
                        self.assertEqual(len(view["Operations"]), 1)
                        self.assertEqual(view["Operations"][0]["ToolName"], "read")
                        self.assertEqual(view["Operations"][0]["Status"], "completed")
                        self.assertIn("DAILY_SOURCE_MARKER", json.dumps(view["Operations"]))
                    if full:
                        send(b"/permissions\r"); until("Schemas are capabilities")
                        send(b"\x1b[B\x1b[B\r"); until("NO OS SANDBOX")
                        self.assertEqual(inspect(p, sid)["Permissions"]["Mode"], "read_only")
                        send(b"\x1b[B\r"); until("Permissions: Full Access")
                        self.assertEqual(inspect(p, sid)["Permissions"]["Mode"], "full_access")
                    send(b"\x04")
                    self.assertEqual(child.wait(timeout=15), 0)
                    self.assertEqual(gate.peer_pid(p["socket"]), pid, "detach killed Host")
                    return p, inspect(p, sid)
                finally:
                    if child.poll() is None: child.kill()
                    child.wait(timeout=10); os.close(master)

            try:
                terminal(repos[0], "")
                first, history = terminal(repos[0], "development", generate=meta["sandbox"] == "devlore", full=True)
                _, again = terminal(repos[0], "development", expected_mode="full_access")
                self.assertEqual(again["Generation"], history["Generation"])
                self.assertEqual(again["Operations"], history["Operations"])
                self.assertEqual(again["History"]["Items"], history["History"]["Items"])
                second, other = terminal(repos[1], "development")
                self.assertNotEqual(owners[str(first["socket"])], owners[str(second["socket"])])
                self.assertFalse(other.get("Operations"))
                stop_owner(first)
                _, resumed = terminal(repos[0], "development")
                self.assertEqual(resumed["Operations"], history["Operations"])
                self.assertEqual(resumed["History"]["Items"][:len(history["History"]["Items"])], history["History"]["Items"])
                self.assertEqual({p: (p.read_bytes(), p.stat().st_mtime_ns) for p in before}, before)
                for root in repos:
                    self.assertEqual((root / "source.txt").read_bytes(), b"DAILY_SOURCE_MARKER\n")
                if meta["sandbox"] == "devlore":
                    calls = [json.loads(line) for line in (user_home / "fake/calls.jsonl").read_text().splitlines()]
                    self.assertEqual(len(calls), 2, "attach/resume must not replay model generations")
                    self.assertTrue(all(Path(c["Directory"]).name.startswith("unreal-claude-structured-") for c in calls))
                    self.assertTrue(all(Path(c["Directory"]) not in repos for c in calls), "Claude must stay in its isolated protocol cwd")
            finally:
                for root in repos:
                    stop_owner(daily.repo_paths(user_home, meta["sandbox"], root))
                libc.prctl(36, old_subreaper.value, 0, 0, 0)


if __name__ == "__main__":
    unittest.main()

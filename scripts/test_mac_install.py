import os
from pathlib import Path
import shutil
import stat
import subprocess
import tempfile
import unittest


SCRIPT = Path(__file__).with_name("mac") / "install.sh"
# The real make build writes four files; the fixture does the same, cheaply.
MAKEFILE = """\
build:
\tmkdir -p "$(BIN_DIR)"
\tprintf '#!/bin/sh\\necho "built unreal-agent $$*"\\n' > "$(BIN_DIR)/unreal-agent"
\tchmod 0755 "$(BIN_DIR)/unreal-agent"
\tln -sf unreal-agent "$(BIN_DIR)/unreal"
\tprintf '#!/bin/sh\\nexit 0\\n' > "$(BIN_DIR)/unreal-agent-runner"
\tchmod 0755 "$(BIN_DIR)/unreal-agent-runner"
\tprintf '#!/bin/sh\\nexit 0\\n' > "$(BIN_DIR)/unreal-agent-auth"
\tchmod 0755 "$(BIN_DIR)/unreal-agent-auth"
"""
BASE_PATH = ("/usr/bin", "/bin", "/usr/sbin", "/sbin", "/usr/local/bin", "/opt/homebrew/bin")


class MacInstallTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="mac-install-")
        self.addCleanup(self.temporary.cleanup)
        # git reports resolved roots; the macOS temporary directory is a symlink.
        self.home = Path(self.temporary.name).resolve()
        self.tmpdir = self.home / "tmp"
        self.tmpdir.mkdir()
        self.tools = self.home / "toolbin"
        self.tools.mkdir()
        self.repo = self.home / "unreal-agent"
        (self.repo / "scripts/mac").mkdir(parents=True)
        shutil.copy2(SCRIPT, self.repo / "scripts/mac/install.sh")
        (self.repo / "Makefile").write_text(MAKEFILE)
        (self.repo / "internal/buildfeatures").mkdir(parents=True)
        (self.repo / "internal/buildfeatures/opencode_go_disabled.go").write_text("// fixture\n")
        self.env = {
            "HOME": str(self.home),
            "TMPDIR": str(self.tmpdir),
            "PATH": self.path(),
            "PYTHONDONTWRITEBYTECODE": "1",
            "GIT_CONFIG_NOSYSTEM": "1",
            "GIT_TERMINAL_PROMPT": "0",
        }
        self.git("init", "--quiet", ".")
        self.commit("initial")

    @property
    def opt(self):
        return self.home / ".local/opt/unreal-agent"

    @property
    def current(self):
        return self.opt / "current"

    @property
    def local_bin(self):
        return self.home / ".local/bin"

    @property
    def wrapper(self):
        return self.local_bin / "unreal"

    def path(self, local_bin=False):
        parts = [str(self.tools)]
        if local_bin:
            parts.append(str(self.home / ".local/bin"))
        return ":".join(parts + list(BASE_PATH))

    def env_for(self, local_bin=False):
        env = dict(self.env)
        env["PATH"] = self.path(local_bin)
        return env

    def git(self, *args):
        return subprocess.run(["git", *args], cwd=str(self.repo), env=self.env, check=True,
                              stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)

    def commit(self, message):
        with open(self.repo / "revisions.log", "a") as log:
            log.write(message + "\n")
        self.git("add", "scripts/mac/install.sh", "Makefile", "revisions.log", "internal/buildfeatures")
        self.git("-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid",
                 "-c", "commit.gpgsign=false", "commit", "--quiet", "-m", message)
        return self.git("rev-parse", "HEAD").stdout.strip()

    def install(self, revision, local_bin=False, check=True, disable_opencode_go=False):
        command = ["sh", "scripts/mac/install.sh", revision]
        if disable_opencode_go:
            command.append("--disable-opencode-go")
        result = subprocess.run(command, cwd=str(self.repo),
                                env=self.env_for(local_bin), stdout=subprocess.PIPE,
                                stderr=subprocess.PIPE, text=True)
        if check:
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        return result

    def fake_agent(self, revision):
        script = self.opt / revision / "unreal-agent"
        script.write_text('#!/bin/sh\nprintf \'agent %s\\n\' "' + revision + '"\nprintf \'%s\\n\' "$@"\n')
        script.chmod(0o755)

    def test_install_builds_switches_and_writes_wrapper(self):
        revision = self.commit("first")
        rev7 = revision[:7]
        result = self.install(revision)
        self.assertIn('~/.local/bin is not on PATH', result.stdout)
        self.assertIn('add the following line to your shell profile', result.stdout)
        self.assertIn('export PATH="$HOME/.local/bin:$PATH"', result.stdout)
        version = self.opt / rev7
        self.assertTrue(version.is_dir())
        for name in ("unreal-agent", "unreal", "unreal-agent-runner", "unreal-agent-auth"):
            self.assertTrue((version / name).exists(), name)
        self.assertTrue(os.access(version / "unreal-agent", os.X_OK))
        self.assertEqual(os.readlink(version / "unreal"), "unreal-agent")
        self.assertEqual(os.readlink(self.current), rev7)
        self.assertTrue(self.wrapper.is_file())
        self.assertEqual(stat.S_IMODE(self.wrapper.stat().st_mode), 0o755)
        content = self.wrapper.read_text()
        self.assertIn("unreal-agent wrapper", content)
        self.assertIn('exec "$OPT/$(readlink "$OPT/current")/unreal-agent" folders "$@"', content)
        self.assertFalse((self.home / ".zshrc").exists())
        # With ~/.local/bin on PATH the hint is no longer printed.
        again = self.install(revision, local_bin=True)
        self.assertNotIn("is not on PATH", again.stdout)
        self.assertNotIn("export PATH=", again.stdout)
        self.assertEqual(list(path.name for path in self.tmpdir.iterdir()), [])

    def test_wrapper_execs_current_version_under_folders(self):
        first = self.commit("wrapper-1")
        second = self.commit("wrapper-2")
        self.install(first, local_bin=True)
        self.install(second, local_bin=True)
        self.fake_agent(first[:7])
        self.fake_agent(second[:7])
        self.assertEqual(os.readlink(self.current), second[:7])
        result = subprocess.run([str(self.wrapper), "chat", "--model", "fixture"], env=self.env_for(local_bin=True),
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        lines = result.stdout.splitlines()
        self.assertEqual(lines[0], "agent " + second[:7])
        self.assertEqual(lines[1], "folders")
        self.assertEqual(lines[2:], ["chat", "--model", "fixture"])
        self.assertFalse((self.home / ".zshrc").exists())

    def test_same_revision_is_not_rebuilt(self):
        revision = self.commit("same")
        rev7 = revision[:7]
        self.install(revision, local_bin=True)
        marker = self.opt / rev7 / "unreal-agent"
        marker.write_text("#!/bin/sh\necho marker\n")
        marker.chmod(0o755)
        self.install(revision, local_bin=True)
        self.assertEqual(marker.read_text(), "#!/bin/sh\necho marker\n")
        self.assertEqual(os.readlink(self.current), rev7)
        self.assertEqual(sorted(path.name for path in self.opt.iterdir()), sorted([rev7, "current"]))
        self.assertFalse((self.home / ".zshrc").exists())

    def test_three_revisions_keep_current_and_previous(self):
        revisions = [self.commit("rev%d" % index) for index in (1, 2, 3)]
        self.install(revisions[0], local_bin=True)
        self.install(revisions[1], local_bin=True)
        result = self.install(revisions[2], local_bin=True)
        self.assertIn("removed " + revisions[0][:7], result.stdout)
        self.assertEqual(os.readlink(self.current), revisions[2][:7])
        directories = sorted(path.name for path in self.opt.iterdir()
                              if path.is_dir() and path.name != "current")
        self.assertEqual(directories, sorted([revisions[1][:7], revisions[2][:7]]))
        self.assertEqual(list(path.name for path in self.tmpdir.iterdir()), [])
        self.assertFalse((self.home / ".zshrc").exists())

    def test_reinstall_of_current_version_keeps_the_previous_one(self):
        first = self.commit("reinstall-1")
        second = self.commit("reinstall-2")
        self.install(first, local_bin=True)
        self.install(second, local_bin=True)
        self.assertEqual(os.readlink(self.current), second[:7])
        marker = self.opt / second[:7] / "unreal-agent"
        marker.write_text("#!/bin/sh\necho marker\n")
        marker.chmod(0o755)
        # The installed version is already current, so the versions to keep have
        # not changed: pruning here used to drop the real previous version,
        # because `previous` has just been read back as the version being installed.
        result = self.install(second, local_bin=True)
        self.assertIn("version " + second[:7] + " is already installed", result.stdout)
        self.assertEqual(marker.read_text(), "#!/bin/sh\necho marker\n")
        self.assertEqual(os.readlink(self.current), second[:7])
        directories = sorted(path.name for path in self.opt.iterdir()
                              if path.is_dir() and path.name != "current")
        self.assertEqual(directories, sorted([first[:7], second[:7]]))
        self.assertEqual(list(path.name for path in self.tmpdir.iterdir()), [])
        self.assertFalse((self.home / ".zshrc").exists())

    def test_disabled_variant_does_not_reuse_enabled_binary(self):
        revision = self.commit("variants")
        self.install(revision)
        enabled = self.opt / revision[:7] / "unreal-agent"
        enabled.write_text("#!/bin/sh\necho enabled-marker\n")
        self.install(revision, disable_opencode_go=True)
        self.assertEqual(os.readlink(self.current), revision[:7] + "-no-opencode-go")
        disabled = self.opt / (revision[:7] + "-no-opencode-go") / "unreal-agent"
        self.assertTrue(disabled.exists())
        self.assertNotIn("enabled-marker", disabled.read_text())
        self.assertIn("enabled-marker", enabled.read_text())

    def test_foreign_wrapper_is_refused_without_changes(self):
        self.local_bin.mkdir(parents=True)
        self.wrapper.write_text("#!/bin/sh\necho owned by another program\n")
        self.wrapper.chmod(0o755)
        before = self.wrapper.read_bytes()
        revision = self.commit("foreign")
        result = self.install(revision, local_bin=True, check=False)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("nothing changed", result.stderr)
        self.assertEqual(self.wrapper.read_bytes(), before)
        self.assertFalse(self.opt.exists())
        self.assertFalse(self.current.is_symlink())
        self.assertEqual(list(path.name for path in self.tmpdir.iterdir()), [])
        self.assertFalse((self.home / ".zshrc").exists())


if __name__ == "__main__":
    unittest.main()

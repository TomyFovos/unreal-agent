import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).parent))
import daily_development as daily
import common_development as common
import pilot_provider as pilot


class DailyDevelopmentTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="dd-", dir="/tmp")
        self.addCleanup(self.temp.cleanup)
        self.home = Path(self.temp.name)
        self.env = {"HOME": str(self.home), "PATH": "/usr/bin:/bin", "PYTHONDONTWRITEBYTECODE": "1"}

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
        paths = [common.repo_paths(self.home, r) for r in roots]
        self.assertEqual(len({p["socket"] for p in paths}), 3)
        self.assertEqual(len({p["state"] for p in paths}), 3)

    def test_nonrepo_bare_overrides_and_private_storage_rejected(self):
        nonrepo = self.home / "workspace"; nonrepo.mkdir()
        bare = self.home / "bare"; subprocess.run(["git", "init", "--bare", "--quiet", str(bare)], check=True, env=self.env)
        # Like Codex, a directory outside a work tree is its own workspace.
        for path in (nonrepo, bare):
            self.assertEqual(self.root(path), path.resolve())
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
        with self.assertRaises(pilot.PilotError): common.repo_paths(Path("/" + "x" * 100), self.home)
        self.assertEqual(daily.safe_path("/tmp/日本語 path"), Path("/tmp/日本語 path"))


if __name__ == "__main__":
    unittest.main()

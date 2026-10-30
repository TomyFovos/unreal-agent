"""Cross-workspace launcher tests use only synthetic state and a native stub."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

import common_development as common
import pilot_provider as pilot


class SessionDiscoveryTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="us-", dir="/tmp")
        self.addCleanup(self.temp.cleanup)
        self.home = Path(self.temp.name)
        self.bin = self.home / "bin"
        self.bin.mkdir()
        native = self.bin / "unreal"
        native.write_text("#!" + sys.executable + "\nimport json,os,sys\n"
                          "print('NATIVE:'+json.dumps({'cwd':os.getcwd(),'args':sys.argv[1:]}))\n")
        native.chmod(0o700)
        self.settings = dict(Version=3, Home=str(self.home), BinaryDirectory=str(self.bin),
                             Revision="a"*40, DockerOAuth=False, CodexRoute="external",
                             ClaudeDecision="enabled", ClaudeBinary="")
        common.settings_path(self.home).parent.mkdir(parents=True, mode=0o700)
        pilot.publish_json(common.settings_path(self.home), self.settings)
        self.outside = self.home / "outside"
        self.outside.mkdir()
        self.env = dict(os.environ, HOME=str(self.home), PYTHONDONTWRITEBYTECODE="1")
        for key in ("GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE"):
            self.env.pop(key, None)

    def register(self, directory, name, when=1000, instance=None):
        workspace = self.home / directory
        workspace.mkdir(parents=True, exist_ok=True)
        paths = common.select_repo(self.settings, workspace, None)
        if instance:
            paths = common.prepare_repo(self.settings, workspace, instance)
        sessions = paths["state"] / "sessions"
        sessions.mkdir(parents=True, exist_ok=True)
        path = sessions / (name + ".session.jsonl")
        # Discovery must never decode these deliberately invalid histories.
        path.write_bytes(b"SESSION_CONTENT_MUST_NOT_BE_READ")
        path.chmod(0)
        os.utime(path, (when, when))
        return workspace, paths

    def launch(self, *args, cwd=None, choice=""):
        return subprocess.run([sys.executable, "-B", str(Path(common.__file__).resolve()), "launch", *args],
                              cwd=cwd or self.outside, env=self.env, input=choice,
                              capture_output=True, text=True, timeout=10)

    def native(self, result):
        self.assertEqual(result.returncode, 0, result.stderr)
        return json.loads(next(row[7:] for row in result.stdout.splitlines() if row.startswith("NATIVE:")))

    def test_named_session_from_unrelated_directory(self):
        workspace, paths = self.register("work space", "development")
        result = self.native(self.launch("development"))
        self.assertEqual(result["cwd"], str(workspace))
        self.assertEqual(result["args"][-1], "development")
        self.assertEqual(result["args"][1], str(paths["state"]))

    def test_duplicate_name_asks_for_workspace(self):
        older, _ = self.register("older", "dev", 1000)
        newer, _ = self.register("newer", "dev", 2000)
        result = self.launch("dev", choice="2\n")
        self.assertEqual(self.native(result)["cwd"], str(older))
        self.assertIn(str(newer), result.stderr)
        self.assertIn(str(older), result.stderr)

    def test_bare_launcher_lists_recent_main_sessions(self):
        older, _ = self.register("older", "old", 1000)
        newer, _ = self.register("newer", "recent", 2000)
        self.register("newer", "child-hidden", 3000)
        result = self.launch(choice="1\n")
        self.assertEqual(self.native(result)["cwd"], str(newer))
        self.assertLess(result.stderr.index("recent"), result.stderr.index("old"))
        self.assertIn(str(older), result.stderr)
        self.assertIn("1970-", result.stderr)
        self.assertNotIn("child-hidden", result.stderr)

    def test_where_prints_only_path_without_changing_state(self):
        workspace, _ = self.register("work space", "dev")
        before = {p: (p.stat().st_mtime_ns, p.stat().st_size) for p in self.home.rglob("*") if p.is_file()}
        result = self.launch("--where", "dev")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, str(workspace)+"\n")
        after = {p: (p.stat().st_mtime_ns, p.stat().st_size) for p in self.home.rglob("*") if p.is_file()}
        self.assertEqual(before, after)
        missing = self.launch("--where", "missing")
        self.assertNotEqual(missing.returncode, 0)
        self.assertEqual(missing.stdout, "")

    def test_registered_workspace_keeps_local_creation_and_default(self):
        here, paths = self.register("here", "existing")
        self.register("elsewhere", "dev")
        nested = here / "nested"
        nested.mkdir()
        result = self.native(self.launch("dev", cwd=nested))
        self.assertEqual(result["cwd"], str(here))
        self.assertEqual(result["args"][1], str(paths["state"]))
        default = self.native(self.launch(cwd=nested))
        self.assertEqual(default["cwd"], str(here))
        self.assertEqual(default["args"][-1], "default")

    def test_unmatched_name_creates_in_current_directory(self):
        self.register("elsewhere", "other")
        result = self.native(self.launch("new"))
        self.assertEqual(result["cwd"], str(self.outside))
        self.assertEqual(result["args"][-1], "new")

    def test_legacy_profile_state_is_used(self):
        workspace, paths = self.register("legacy", "history", instance="p-deadbeef")
        result = self.native(self.launch("history"))
        self.assertEqual(result["cwd"], str(workspace))
        self.assertEqual(result["args"][1], str(paths["state"]))

    def test_where_prefers_current_workspace_and_prompts_elsewhere(self):
        here, _ = self.register("here", "dev", 1000)
        there, _ = self.register("there", "dev", 2000)
        result = self.launch("--where", "dev", cwd=here)
        self.assertEqual(result.stdout, str(here)+"\n")
        result = self.launch("--where", "dev", choice="1\n")
        self.assertEqual(result.stdout, str(there)+"\n")
        self.assertIn(str(here), result.stderr)


if __name__ == "__main__":
    unittest.main()

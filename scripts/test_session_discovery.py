"""Cross-workspace launcher tests use only synthetic state and a native stub."""
from http.server import BaseHTTPRequestHandler
import json
import os
from pathlib import Path
import socket
from socketserver import UnixStreamServer
import subprocess
import sys
import tempfile
from threading import Thread
import unittest

import common_development as common
import pilot_provider as pilot

# The Linux kit reads /proc and refuses symlinked private directories, and
# macOS reaches its temporary directory through /var. These run on Linux CI.
LINUX_ONLY = unittest.skipUnless(sys.platform.startswith("linux"), "Linux-only kit behavior")


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

    @LINUX_ONLY
    def test_named_session_from_unrelated_directory(self):
        workspace, paths = self.register("work space", "development")
        result = self.native(self.launch("development"))
        self.assertEqual(result["cwd"], str(workspace))
        self.assertEqual(result["args"][-1], "development")
        self.assertEqual(result["args"][1], str(paths["state"]))

    @LINUX_ONLY
    def test_duplicate_name_asks_for_workspace(self):
        older, _ = self.register("older", "dev", 1000)
        newer, _ = self.register("newer", "dev", 2000)
        result = self.launch("dev", choice="2\n")
        self.assertEqual(self.native(result)["cwd"], str(older))
        self.assertIn(str(newer), result.stderr)
        self.assertIn(str(older), result.stderr)

    @LINUX_ONLY
    def test_named_session_from_stale_workspace_prefers_running_host(self):
        old, old_paths = self.register("old", "jh", 1000, instance="p-old")
        live, live_paths = self.register("live", "jh", 2000)
        nested = old / "nested"
        nested.mkdir()
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as stale:
            stale.bind(str(old_paths["socket"]))

        class ReadinessHandler(BaseHTTPRequestHandler):
            def do_POST(self):
                if self.path != "/v1/auth/methods":
                    self.send_error(404)
                    return
                self.rfile.read(int(self.headers["Content-Length"]))
                self.send_response(200)
                self.end_headers()
                self.wfile.write(b'{"Methods":[{}]}\n')

            def log_message(self, *args):
                pass

        with UnixStreamServer(str(live_paths["socket"]), ReadinessHandler) as server:
            thread = Thread(target=server.serve_forever, kwargs={"poll_interval": 0.01})
            thread.start()
            try:
                result = self.launch("jh", cwd=nested, choice="\n")
                native = self.native(result)
                self.assertEqual(native["cwd"], str(live))
                self.assertEqual(native["args"][1], str(live_paths["state"]))
                self.assertEqual(native["args"][-1], "jh")
                self.assertIn("番号 [1]", result.stderr)

                # Host status still wins when the stopped copy is newer.
                os.utime(old_paths["state"] / "sessions/jh.session.jsonl", (3000, 3000))
                result = self.launch("jh", cwd=nested, choice="\n")
                self.assertEqual(self.native(result)["cwd"], str(live))
                warning, _, old_row, live_row, prompt = result.stderr.splitlines()
                self.assertIn("同じ名前の Session が 2 個あります", warning)
                self.assertIn(f"1. jh  {old}  1970-", old_row)
                self.assertIn("Host 停止中  instance=p-old", old_row)
                self.assertIn(f"2. jh  {live}  1970-", live_row)
                self.assertIn("Host 稼働中  instance=default", live_row)
                self.assertIn("番号 [2]", prompt)
                override = self.native(self.launch("jh", cwd=nested, choice="1\n"))
                self.assertEqual(override["args"][1], str(old_paths["state"]))
            finally:
                server.shutdown()
                thread.join()

    @LINUX_ONLY
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

    @LINUX_ONLY
    def test_list_prints_sessions_newest_first_without_launching(self):
        older, _ = self.register("older", "alpha", 1000)
        newer, _ = self.register("newer", "beta", 2000)
        self.register("newer", "child-x", 3000)
        result = self.launch("--list")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn("NATIVE:", result.stdout)
        rows = result.stdout.splitlines()
        self.assertEqual([row.split("  ")[0] for row in rows], ["beta", "alpha"])
        self.assertIn(str(newer), rows[0])
        self.assertIn(str(older), rows[1])

    @LINUX_ONLY
    def test_list_without_sessions_says_so(self):
        result = self.launch("--list")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("Session はありません", result.stdout)
        self.assertNotIn("NATIVE:", result.stdout)

    @LINUX_ONLY
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

    @LINUX_ONLY
    def test_registered_workspace_opens_existing_session_elsewhere(self):
        here, _ = self.register("here", "existing", when=2000)
        elsewhere, paths = self.register("elsewhere", "dev", when=1000)
        nested = here / "nested"
        nested.mkdir()
        # A name only in another workspace opens there instead of creating one here.
        result = self.native(self.launch("dev", cwd=nested))
        self.assertEqual(result["cwd"], str(elsewhere))
        self.assertEqual(result["args"][-1], "dev")
        # Both Hosts are stopped, so the newer copy is the default.
        self.register("here", "dev", when=500)
        newest = self.native(self.launch("dev", cwd=nested))
        self.assertEqual(newest["cwd"], str(elsewhere))
        self.assertEqual(newest["args"][1], str(paths["state"]))
        # A bare launch lists every Session, newest first, and never creates one.
        listed = self.native(self.launch(cwd=nested, choice="2\n"))
        self.assertEqual(listed["cwd"], str(elsewhere))
        self.assertEqual(listed["args"][-1], "dev")

    @LINUX_ONLY
    def test_unmatched_name_requires_confirmation(self):
        self.register("elsewhere", "other")
        declined = self.launch("new")
        self.assertNotEqual(declined.returncode, 0)
        self.assertIn("common_session_not_created", declined.stderr)
        self.assertNotIn("NATIVE:", declined.stdout)
        result = self.native(self.launch("new", choice="y\n"))
        self.assertEqual(result["cwd"], str(self.outside))
        self.assertEqual(result["args"][-1], "new")

    @LINUX_ONLY
    def test_explicit_instance_cannot_take_a_name_used_elsewhere(self):
        elsewhere, _ = self.register("elsewhere", "dev")
        here = self.home / "here"
        here.mkdir()
        subprocess.run(["git", "init", "--quiet", str(here)], check=True, env=self.env)
        taken = self.launch("dev", "--instance", "p-new", cwd=here)
        self.assertNotEqual(taken.returncode, 0)
        self.assertIn("common_session_name_taken", taken.stderr)
        self.assertIn(str(elsewhere), taken.stderr)
        self.assertNotIn("NATIVE:", taken.stdout)
        # The Session's own instance still reopens explicitly.
        own = self.native(self.launch("dev", "--instance", "default", cwd=elsewhere))
        self.assertEqual(own["cwd"], str(elsewhere))
        # A free name may still be created with an explicit instance.
        fresh = self.native(self.launch("fresh", "--instance", "p-new", cwd=here))
        self.assertEqual(fresh["args"][-1], "fresh")

    def test_bare_launch_without_sessions_creates_nothing(self):
        result = self.launch()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("common_session_name_required", result.stderr)
        self.assertNotIn("NATIVE:", result.stdout)

    @LINUX_ONLY
    def test_legacy_profile_state_is_used(self):
        workspace, paths = self.register("legacy", "history", instance="p-deadbeef")
        result = self.native(self.launch("history"))
        self.assertEqual(result["cwd"], str(workspace))
        self.assertEqual(result["args"][1], str(paths["state"]))

    @LINUX_ONLY
    def test_where_defaults_to_newest_and_accepts_manual_choice(self):
        here, _ = self.register("here", "dev", 1000)
        there, _ = self.register("there", "dev", 2000)
        result = self.launch("--where", "dev", cwd=here)
        self.assertEqual(result.stdout, str(there)+"\n")
        self.assertIn("番号 [1]", result.stderr)
        self.assertIn(str(here), result.stderr)
        result = self.launch("--where", "dev", choice="2\n")
        self.assertEqual(result.stdout, str(here)+"\n")

    @LINUX_ONLY
    def test_same_workspace_duplicates_can_be_selected_and_archived_by_instance(self):
        workspace, default_paths = self.register("work", "jh", 1000)
        _, newest_paths = self.register("work", "jh", 2000, instance="p-new")
        result = self.launch("jh", cwd=workspace)
        self.assertEqual(self.native(result)["args"][1], str(newest_paths["state"]))
        self.assertIn("instance=default", result.stderr)
        self.assertIn("番号 [1]", result.stderr)

        args = ("--archive-session", "jh", "--workspace", str(workspace))
        ambiguous = self.launch(*args)
        self.assertIn("common_session_archive_ambiguous", ambiguous.stderr)
        archived = self.launch(*args, "--instance", "p-new")
        self.assertEqual(archived.returncode, 0, archived.stderr)
        remaining = self.native(self.launch("jh", cwd=workspace))
        self.assertEqual(remaining["args"][1], str(default_paths["state"]))

    @LINUX_ONLY
    def test_archive_session_retires_only_the_selected_workspace_name(self):
        old, old_paths = self.register("old", "jh", 1000)
        live, _ = self.register("live", "jh", 2000)
        record = old_paths["state"] / "sessions/jh.session.jsonl"
        before = record.stat()
        archived = self.launch("--archive-session", "jh", "--workspace", str(old))
        self.assertEqual(archived.returncode, 0, archived.stderr)
        self.assertNotIn("NATIVE:", archived.stdout)
        listing = self.launch("--list")
        self.assertEqual(listing.returncode, 0, listing.stderr)
        self.assertIn(str(live), listing.stdout)
        self.assertNotIn(str(old), listing.stdout)
        result = self.launch("--where", "jh", cwd=old)
        self.assertEqual(result.stdout, str(live)+"\n")
        self.assertNotIn("番号", result.stderr)

        archived = self.launch("--archive-session", "jh", "--workspace", str(live))
        self.assertEqual(archived.returncode, 0, archived.stderr)
        hidden = self.launch("jh", cwd=old, choice="y\n")
        self.assertIn("common_session_archived", hidden.stderr)
        self.assertNotIn("NATIVE:", hidden.stdout)
        explicit = self.native(self.launch("jh", "--instance", "default", cwd=old))
        self.assertEqual(explicit["args"][1], str(old_paths["state"]))
        after = record.stat()
        self.assertEqual((after.st_ino, after.st_size, after.st_mtime_ns),
                         (before.st_ino, before.st_size, before.st_mtime_ns))


if __name__ == "__main__":
    unittest.main()

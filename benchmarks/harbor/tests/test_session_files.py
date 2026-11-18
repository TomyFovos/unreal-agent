import json
import unittest
from pathlib import Path
from tempfile import TemporaryDirectory

from harness_harbor.session_files import session_paths


class SessionFilesTests(unittest.TestCase):
    def test_live_archived_and_pending_records_are_each_resolved_once(self):
        with TemporaryDirectory() as temporary:
            directory = Path(temporary) / "sessions"
            archive = Path(temporary) / "sessions-archive"
            directory.mkdir()
            (directory / "main.session.jsonl").write_text("main")
            expected = {"main": directory / "main.session.jsonl"}
            for child, kind, moved in (
                ("finished", "index", True),
                ("pending-live", "pending", False),
                ("pending-moved", "pending", True),
            ):
                index = archive / kind / f"{child}.json"
                index.parent.mkdir(parents=True, exist_ok=True)
                index.write_text(json.dumps({"child_id": child, "parent_id": "main"}))
                path = (
                    archive / "main" / child / f"{child}.session.jsonl"
                    if moved
                    else directory / f"{child}.session.jsonl"
                )
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text(child)
                expected[child] = path
            self.assertEqual(session_paths(directory), expected)
            self.assertEqual(
                sorted(path.read_text() for path in session_paths(directory).values()),
                ["finished", "main", "pending-live", "pending-moved"],
            )

    def test_no_archive_preserves_flat_session_collection(self):
        with TemporaryDirectory() as temporary:
            directory = Path(temporary)
            path = directory / "child.session.jsonl"
            path.write_text("child")
            self.assertEqual(session_paths(directory), {"child": path})

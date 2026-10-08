import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import tarfile
import tempfile
import unittest
from unittest.mock import patch


spec = importlib.util.spec_from_file_location("snapshot", Path(__file__).with_name("snapshot.py"))
snapshot = importlib.util.module_from_spec(spec)
spec.loader.exec_module(snapshot)


class SnapshotTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)

    def environment(self, output):
        return {
            "CGO_ENABLED": "1", "UNREAL_SNAPSHOT_IS_SNAPSHOT": "true",
            "UNREAL_SNAPSHOT_ROOT": str(output), "UNREAL_SNAPSHOT_VERSION": "0.2.1-snapshot-test",
            "UNREAL_SNAPSHOT_COMMIT": "a" * 40, "UNREAL_SNAPSHOT_DATE": "2026-10-08T00:00:00Z",
        }

    @staticmethod
    def fake_command(args, root, env):
        if args[:2] == ["git", "rev-parse"]:
            return "a" * 40
        if args[:2] == ["git", "status"]:
            return "?? existing-private-source.go"
        return "go version go1.27.1 linux/amd64"

    def test_fresh_outputs_preserve_existing_artifacts(self):
        first = snapshot.reserve_output(self.root)
        sentinel = first / "previous-artifact"
        sentinel.write_bytes(b"keep this")
        second = snapshot.reserve_output(self.root)
        self.assertNotEqual(first, second)
        self.assertEqual(sentinel.read_bytes(), b"keep this")
        self.assertEqual(second.stat().st_mode & 0o777, 0o700)

    def test_output_parent_symlink_rejected(self):
        target = self.root / "protected"
        target.mkdir()
        (self.root / "dist").symlink_to(target, target_is_directory=True)
        with self.assertRaises(ValueError):
            snapshot.reserve_output(self.root)
        self.assertEqual(list(target.iterdir()), [])

    def test_guard_rejects_release_and_cgo_disabled_without_writes(self):
        output = snapshot.reserve_output(self.root)
        for key, value in [("UNREAL_SNAPSHOT_IS_SNAPSHOT", "false"), ("CGO_ENABLED", "0"),
                           ("UNREAL_SNAPSHOT_ROOT", str(self.root))]:
            env = self.environment(output)
            env[key] = value
            with self.subTest(key=key), self.assertRaises(ValueError):
                snapshot.prepare(self.root, env)
        self.assertEqual(list(output.iterdir()), [])

    def test_metadata_identifies_dirty_worktree_without_listing_inputs(self):
        output = snapshot.reserve_output(self.root)
        with patch.object(snapshot, "command", side_effect=self.fake_command):
            snapshot.prepare(self.root, self.environment(output))
        raw = (output / "package" / "SNAPSHOT.json").read_text()
        metadata = json.loads(raw)
        self.assertTrue(metadata["worktree_dirty"])
        self.assertTrue(metadata["cgo_enabled"])
        self.assertFalse(metadata["formal_release"])
        self.assertEqual(metadata["repository"], "TomyFovos/unreal-agent")
        self.assertNotIn("existing-private-source", raw)
        with patch.object(snapshot, "command", side_effect=self.fake_command), self.assertRaises(FileExistsError):
            snapshot.prepare(self.root, self.environment(output))
        self.assertEqual((output / "package" / "SNAPSHOT.json").read_text(), raw)

    def test_build_uses_checked_snapshot_only_without_clean_or_publish(self):
        calls = []
        (self.root / ".goreleaser.yaml").write_text("version: 2\ndist: dist/snapshot-direct\n")
        with patch.object(snapshot, "preflight"), patch.object(snapshot.subprocess, "run", side_effect=lambda *a, **kw: calls.append((a, kw))), \
                patch.object(snapshot, "inspect_archive", return_value=(self.root / "archive", {})), \
                patch.object(snapshot, "smoke_archive"):
            output = snapshot.build(self.root, "fake-goreleaser")
        self.assertEqual(calls[0][0][0][:2], ["fake-goreleaser", "check"])
        self.assertIn("--snapshot", calls[1][0][0])
        self.assertIn("--skip=publish", calls[1][0][0])
        self.assertNotIn("--clean", calls[1][0][0])
        self.assertEqual(calls[1][1]["env"]["CGO_ENABLED"], "1")
        self.assertEqual(calls[1][1]["env"]["UNREAL_SNAPSHOT_ROOT"], str(output))

    def test_effective_configuration_changes_only_output_path(self):
        original = "version: 2\ndist: dist/snapshot-direct\nrelease:\n  disable: true\n"
        (self.root / ".goreleaser.yaml").write_text(original)
        output = snapshot.reserve_output(self.root)
        effective = snapshot.configuration(self.root, output).read_text()
        self.assertEqual(effective, original.replace("dist: dist/snapshot-direct", "dist: " + json.dumps(str(output / "artifacts"))))
        self.assertEqual((self.root / ".goreleaser.yaml").read_text(), original)

    def archive(self, extra=None):
        artifacts = self.root / "artifacts"
        artifacts.mkdir()
        archive = artifacts / "snapshot.tar.gz"
        metadata = {"kind": "local-snapshot", "formal_release": False, "target": "linux/amd64", "cgo_enabled": True}
        with tarfile.open(archive, "w:gz") as bundle:
            for name in sorted(snapshot.PACKAGE_FILES | ({extra} if extra else set())):
                data = json.dumps(metadata).encode() if name == "SNAPSHOT.json" else b"dummy"
                member = tarfile.TarInfo(name)
                member.mode = 0o755 if name in snapshot.BINARIES else 0o644
                member.size = len(data)
                bundle.addfile(member, io.BytesIO(data))
        digest = hashlib.sha256(archive.read_bytes()).hexdigest()
        (artifacts / "SHA256SUMS").write_text(f"{digest}  {archive.name}\n")
        return artifacts, archive

    def test_archive_inventory_and_checksum(self):
        artifacts, archive = self.archive()
        self.assertEqual(snapshot.inspect_archive(artifacts)[0], archive)

    def test_private_or_unrelated_file_is_not_packaged(self):
        artifacts, _ = self.archive("credential.json")
        with self.assertRaisesRegex(ValueError, "contents"):
            snapshot.inspect_archive(artifacts)

    def test_archive_path_traversal_rejected(self):
        artifacts, _ = self.archive("../protected")
        with self.assertRaises(ValueError):
            snapshot.inspect_archive(artifacts)

    def test_corrupt_archive_does_not_reach_smoke(self):
        artifacts, archive = self.archive()
        with archive.open("ab") as stream:
            stream.write(b"corrupt")
        with self.assertRaisesRegex(ValueError, "checksum"):
            snapshot.inspect_archive(artifacts)


if __name__ == "__main__":
    unittest.main()

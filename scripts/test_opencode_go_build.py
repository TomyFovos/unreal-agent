import argparse
import io
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import common_development as common
import common_development_bundle as bundle
import pilot_kit
import pilot_provider
import snapshot
from test_pilot_kit import source_archive
import test_snapshot as snapshot_tests


class OpenCodeGoBuildTests(unittest.TestCase):
    def test_snapshot_records_default_and_disabled_and_passes_build_choice(self):
        for enabled in (True, False):
            with self.subTest(enabled=enabled), tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary)
                output = snapshot.reserve_output(root)
                fixture = snapshot_tests.SnapshotTests()
                env = fixture.environment(output)
                env["OPENCODE_GO_ENABLED"] = str(enabled).lower()
                with patch.object(snapshot, "command", side_effect=fixture.fake_command):
                    snapshot.prepare(root, env)
                self.assertIs(json.loads((output / "package/SNAPSHOT.json").read_bytes())["opencode_go_enabled"], enabled)
                (root / ".goreleaser.yaml").write_text("version: 2\ndist: dist/snapshot-direct\n")
                with patch.object(snapshot.subprocess, "run") as run, \
                        patch.object(snapshot, "inspect_archive", return_value=(root / "archive", {})), \
                        patch.object(snapshot, "write_archive_checksum"), patch.object(snapshot, "smoke_archive"), \
                        patch("sys.stdout", new=io.StringIO()):
                    snapshot.build(root, "fake-goreleaser", not enabled)
                self.assertTrue(all(c.kwargs["env"]["OPENCODE_GO_ENABLED"] == str(enabled).lower() for c in run.call_args_list))

    def test_repackaging_preserves_choice_and_refuses_enabled_input_when_disabled_requested(self):
        for enabled in (True, False):
            with self.subTest(enabled=enabled), tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary)
                source = source_archive(root, revision="a" * 40, opencode_go_enabled=enabled)
                output = root / "kit"
                output.mkdir()
                with patch.object(pilot_kit, "binary_requirements", return_value="2.34"):
                    if enabled:
                        with self.assertRaisesRegex(ValueError, "rebuild.*--disable-opencode-go"):
                            bundle.assemble(source, output, "a" * 40, disable_opencode_go=True)
                        self.assertEqual(list(output.iterdir()), [])
                    archive = bundle.assemble(source, output, "a" * 40, disable_opencode_go=not enabled)
                self.assertTrue(archive.is_file())
                package = output / "unreal-agent-linux-amd64-aaaaaaa"
                for name in ("KIT.json", "SNAPSHOT.json"):
                    self.assertIs(json.loads((package / name).read_bytes())["opencode_go_enabled"], enabled)

    def test_disabled_installer_refuses_file_and_environment_before_reading_or_installing(self):
        secret = "synthetic-opencode-secret-never-print"
        for source in ("file", "env"):
            args = argparse.Namespace(opencode_go_api_key_file="", opencode_go_api_key_env="")
            setattr(args, "opencode_go_api_key_" + source, secret)
            with self.subTest(source=source), patch.dict(os.environ, {secret: secret}), \
                    patch.object(common.daily, "home", side_effect=AssertionError("installation started")), \
                    patch.object(Path, "read_bytes", side_effect=AssertionError("key file read")), \
                    self.assertRaises(pilot_provider.PilotError) as failure:
                common.install(args, Path("/unused"), {"opencode_go_enabled": False})
            self.assertIn("この配布物では OpenCode Go は使えません", str(failure.exception))
            self.assertNotIn(secret, str(failure.exception))

    def test_enabled_installer_accepts_explicit_key_source(self):
        with tempfile.TemporaryDirectory() as temporary:
            key = Path(temporary) / "key"
            key.write_bytes(b"synthetic-key")
            key.chmod(0o600)
            args = argparse.Namespace(opencode_go_api_key_file=str(key), opencode_go_api_key_env="")
            self.assertEqual(common.open_code_go_key(args, {"opencode_go_enabled": True}), b"synthetic-key")
            args.opencode_go_api_key_file, args.opencode_go_api_key_env = "", "SYNTHETIC_OPENCODE_KEY"
            with patch.dict(os.environ, {"SYNTHETIC_OPENCODE_KEY": "synthetic-env-key"}):
                self.assertEqual(common.open_code_go_key(args, {}), b"synthetic-env-key")


if __name__ == "__main__":
    unittest.main()

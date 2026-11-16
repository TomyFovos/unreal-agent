import importlib.util
import io
import json
import tarfile
import unittest
from pathlib import Path
from tempfile import TemporaryDirectory
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location(
    "harbor_build", Path(__file__).resolve().parents[1] / "build.py"
)
builder = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(builder)


class BuildFeatureTests(unittest.TestCase):
    def test_feature_choice_reaches_both_executables_and_manifest(self):
        for cgo in ("0", "1"):
            for enabled in (True, False):
                with (
                    self.subTest(cgo=cgo, enabled=enabled),
                    TemporaryDirectory() as temporary,
                ):
                    output = Path(temporary) / "bundle"
                    commands = []

                    def run(command, **kwargs):
                        if command[:2] == ["git", "archive"]:
                            with tarfile.open(
                                fileobj=kwargs["stdout"], mode="w"
                            ) as tree:
                                for name, data in {
                                    "harness/llm/clients/claudeagent/install.py": (
                                        b'NODE_VERSION="fixture"\n'
                                        b'CLAUDE_VERSION="fixture"\n'
                                    ),
                                    "internal/buildfeatures/opencode_go_disabled.go": (
                                        b"// fixture"
                                    ),
                                }.items():
                                    info = tarfile.TarInfo(name)
                                    info.size = len(data)
                                    tree.addfile(info, io.BytesIO(data))
                        elif command[:2] == ["go", "build"]:
                            commands.append(command)
                            Path(command[command.index("-o") + 1]).write_bytes(
                                b"synthetic executable"
                            )
                        else:
                            self.fail(f"unexpected command {command}")

                    with (
                        patch.dict(builder.os.environ, {"CGO_ENABLED": cgo}),
                        patch.object(builder.subprocess, "run", side_effect=run),
                        patch.object(
                            builder.subprocess,
                            "check_output",
                            side_effect=["a" * 40, "go version fixture"],
                        ),
                        patch("sys.stdout", new=io.StringIO()),
                    ):
                        builder.build(
                            "HEAD", output, "amd64", "unreal-agent-runner", not enabled
                        )
                    self.assertEqual(len(commands), 2)
                    for command in commands:
                        tags = [arg for arg in command if arg.startswith("-tags=")]
                        self.assertEqual(
                            len(tags), 1 if cgo == "1" or not enabled else 0
                        )
                        self.assertEqual(
                            "disable_opencode_go" in ",".join(tags), not enabled
                        )
                        if cgo == "1":
                            self.assertIn("netgo,osusergo", tags[0])
                            self.assertIn(
                                "-ldflags=-linkmode=external -extldflags=-static",
                                command,
                            )
                    manifest = json.loads((output / "manifest.json").read_text())
                    self.assertIs(manifest["opencode_go_enabled"], enabled)


if __name__ == "__main__":
    unittest.main()

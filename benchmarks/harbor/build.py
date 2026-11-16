"""Build a Linux runner bundle from a committed source tree."""

import argparse
import hashlib
import json
import os
import runpy
import subprocess
import tarfile
from contextlib import ExitStack
from pathlib import Path
from tempfile import NamedTemporaryFile, TemporaryDirectory


def build(
    revision: str,
    output: Path,
    arch: str,
    runner: str,
    disable_opencode_go: bool = False,
) -> None:
    repo = Path(__file__).resolve().parents[2]
    commit = subprocess.check_output(
        ["git", "rev-parse", "--verify", f"{revision}^{{commit}}"], cwd=repo, text=True
    ).strip()
    if output.exists():
        raise FileExistsError(f"Refusing to replace runner bundle: {output}")
    with ExitStack() as stack:
        source = Path(stack.enter_context(TemporaryDirectory(prefix="harbor-build-")))
        archive = stack.enter_context(NamedTemporaryFile(suffix=".tar"))
        subprocess.run(["git", "archive", commit], cwd=repo, stdout=archive, check=True)
        archive.flush()
        with tarfile.open(archive.name) as tree:
            tree.extractall(source, filter="data")
        if (
            disable_opencode_go
            and not (
                source / "internal/buildfeatures/opencode_go_disabled.go"
            ).is_file()
        ):
            raise ValueError(
                "This revision cannot disable OpenCode Go; "
                "use a revision with build feature support"
            )
        binary = source / "unreal-agent-runner"
        cgo = os.environ.get("CGO_ENABLED", "1")
        build_flags = []
        if cgo == "1":
            # Keep the native AST parser while avoiding a host-glibc dependency
            # in older benchmark containers. Cross builds require a matching CC.
            build_flags = [
                "-tags=netgo,osusergo",
                "-ldflags=-linkmode=external -extldflags=-static",
            ]
        if disable_opencode_go:
            if cgo == "1":
                build_flags[0] += ",disable_opencode_go"
            else:
                build_flags.append("-tags=disable_opencode_go")
        subprocess.run(
            [
                "go",
                "build",
                "-trimpath",
                "-buildvcs=false",
                *build_flags,
                "-o",
                str(binary),
                f"./cmd/{runner}",
            ],
            cwd=source,
            env={**os.environ, "GOOS": "linux", "GOARCH": arch, "CGO_ENABLED": cgo},
            check=True,
        )
        host_binary = source / "unreal-agent"
        subprocess.run(
            [
                "go",
                "build",
                "-trimpath",
                "-buildvcs=false",
                *build_flags,
                "-o",
                str(host_binary),
                "./cmd/unreal-agent",
            ],
            cwd=source,
            env={**os.environ, "GOOS": "linux", "GOARCH": arch, "CGO_ENABLED": cgo},
            check=True,
        )
        assets = {
            "unreal-agent": host_binary.read_bytes(),
            "claude-install.py": (
                source / "harness/llm/clients/claudeagent/install.py"
            ).read_bytes(),
        }
        pins = runpy.run_path(
            str(source / "harness/llm/clients/claudeagent/install.py")
        )
        data = binary.read_bytes()
        manifest = {
            "runtime": {
                "node": pins["NODE_VERSION"],
                "claude_code": pins["CLAUDE_VERSION"],
            },
            "revision": commit,
            "opencode_go_enabled": not disable_opencode_go,
            "files": {
                name: hashlib.sha256(value).hexdigest()
                for name, value in assets.items()
            },
            "runner": runner,
            "sha256": hashlib.sha256(data).hexdigest(),
            "goos": "linux",
            "goarch": arch,
            "cgo_enabled": cgo,
            "go_version": subprocess.check_output(["go", "version"], text=True).strip(),
        }
        output.mkdir(parents=True)
        (output / "unreal-agent-runner").write_bytes(data)
        (output / "unreal-agent-runner").chmod(0o755)
        for name, value in assets.items():
            (output / name).write_bytes(value)
        (output / "unreal-agent").chmod(0o755)
        (output / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
    print(f"Built {commit} ({manifest['sha256']}) in {output}")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--revision", default="HEAD")
    parser.add_argument("--disable-opencode-go", action="store_true")
    parser.add_argument(
        "--runner",
        default="unreal-agent-runner",
        help="runner command (default: unreal-agent-runner)",
    )
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--arch", choices=("amd64", "arm64"), default="amd64")
    args = parser.parse_args()
    build(args.revision, args.output, args.arch, args.runner, args.disable_opencode_go)

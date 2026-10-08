#!/usr/bin/env python3
"""Build an isolated, non-publishing Linux amd64 snapshot of the Fork."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import shutil
import subprocess
import sys
import tarfile
import tempfile


ROOT = Path(__file__).resolve().parents[1]
GORELEASER_VERSION = "2.18.2"
BINARIES = ("unreal", "unreal-agent", "unreal-agent-runner", "unreal-agent-auth")
PACKAGE_FILES = set(BINARIES) | {"LICENSE", "SNAPSHOT.json", "docs/distribution.md"}


def command(args, root, env):
    return subprocess.check_output(args, cwd=root, env=env, text=True).strip()


def preflight(root, executable, env):
    version = command([executable, "--version"], root, env)
    if not re.search(r"GitVersion:\s+" + re.escape(GORELEASER_VERSION) + r"\s", version + "\n"):
        raise ValueError(f"use GoReleaser {GORELEASER_VERSION}")
    if command(["go", "env", "GOHOSTOS", "GOHOSTARCH"], root, env).splitlines() != ["linux", "amd64"]:
        raise ValueError("this snapshot target requires a native Linux amd64 Go toolchain")
    compiler = shlex.split(command(["go", "env", "CC"], root, env))
    if not compiler or not shutil.which(compiler[0], path=env.get("PATH")):
        raise ValueError("CGO requires a working C compiler")


def reserve_output(root):
    # Never accept an existing output directory or use GoReleaser --clean.
    parent = root / "dist"
    snapshots = parent / "snapshots"
    for path in (parent, snapshots):
        if path.is_symlink():
            raise ValueError("snapshot output parents must not be symlinks")
        path.mkdir(mode=0o700, exist_ok=True)
    return Path(tempfile.mkdtemp(prefix="snapshot-", dir=snapshots))


def configuration(root, output):
    contents = (root / ".goreleaser.yaml").read_text(encoding="utf-8")
    if len(re.findall(r"^dist:.*$", contents, re.MULTILINE)) != 1:
        raise ValueError("expected one top-level GoReleaser dist path")
    contents = re.sub(r"^dist:.*$", lambda _: "dist: " + json.dumps(str(output / "artifacts")),
                      contents, count=1, flags=re.MULTILINE)
    path = output / "goreleaser.yaml"
    with path.open("x", encoding="utf-8") as stream:
        stream.write(contents)
    return path


def prepare(root, env):
    if env.get("UNREAL_SNAPSHOT_IS_SNAPSHOT") != "true":
        raise ValueError("this configuration only supports --snapshot")
    if env.get("CGO_ENABLED") != "1":
        raise ValueError("CGO_ENABLED=1 is required for AST")
    value = env.get("UNREAL_SNAPSHOT_ROOT", "")
    output = Path(value)
    parent = root / "dist" / "snapshots"
    if (not output.is_absolute() or output.parent != parent
            or not output.name.startswith("snapshot-") or output.is_symlink()
            or not output.is_dir() or parent.is_symlink() or parent.parent.is_symlink()):
        raise ValueError("run make snapshot to reserve a fresh private output directory")
    commit = command(["git", "rev-parse", "HEAD"], root, env)
    if commit != env.get("UNREAL_SNAPSHOT_COMMIT"):
        raise ValueError("snapshot revision changed before build")
    metadata = {
        "format_version": 1,
        "repository": "TomyFovos/unreal-agent",
        "kind": "local-snapshot",
        "version": env["UNREAL_SNAPSHOT_VERSION"],
        "revision": commit,
        "built_at": env["UNREAL_SNAPSHOT_DATE"],
        "worktree_dirty": bool(command(["git", "status", "--porcelain", "--untracked-files=all"], root, env)),
        "source_scope": "current worktree, including uncommitted sources",
        "target": "linux/amd64",
        "cgo_enabled": True,
        "go_version": command(["go", "version"], root, env),
        "formal_release": False,
    }
    package = output / "package"
    package.mkdir(mode=0o700)
    with (package / "SNAPSHOT.json").open("x", encoding="utf-8") as stream:
        json.dump(metadata, stream, ensure_ascii=True, indent=2)
        stream.write("\n")


def inspect_archive(artifacts):
    archives = list(artifacts.glob("*.tar.gz"))
    if len(archives) != 1:
        raise ValueError("expected exactly one Linux amd64 archive")
    archive = archives[0]
    sums = (artifacts / "SHA256SUMS").read_text(encoding="ascii").splitlines()
    expected = None
    for line in sums:
        digest, name = line.split(maxsplit=1)
        name = name.lstrip(" *")
        if not re.fullmatch(r"[0-9a-f]{64}", digest) or Path(name).name != name:
            raise ValueError("invalid checksum entry")
        with (artifacts / name).open("rb") as stream:
            if hashlib.file_digest(stream, "sha256").hexdigest() != digest:
                raise ValueError("snapshot checksum mismatch")
        if name == archive.name:
            expected = digest
    if expected is None:
        raise ValueError("archive checksum missing")
    with tarfile.open(archive, "r:gz") as bundle:
        members = bundle.getmembers()
        if (len(members) != len(PACKAGE_FILES)
                or {member.name for member in members} != PACKAGE_FILES
                or any(not member.isfile() for member in members)):
            raise ValueError("unexpected snapshot archive contents")
        for name in BINARIES:
            if bundle.getmember(name).mode & 0o111 == 0:
                raise ValueError("snapshot binary is not executable")
        with bundle.extractfile("SNAPSHOT.json") as stream:
            metadata = json.load(stream)
        if (metadata.get("kind") != "local-snapshot" or metadata.get("formal_release") is not False
                or metadata.get("target") != "linux/amd64" or metadata.get("cgo_enabled") is not True):
            raise ValueError("invalid snapshot provenance")
    return archive, metadata


def smoke_archive(archive, metadata, root, env):
    # Extract only inspected regular members into a new directory; no local
    # config/authentication, project files, or real inference is used.
    with tempfile.TemporaryDirectory(prefix="unreal-snapshot-smoke-") as temporary:
        directory = Path(temporary)
        with tarfile.open(archive, "r:gz") as bundle:
            bundle.extractall(directory, filter="data")
        smoke_env = {"PATH": env["PATH"], "HOME": str(directory / "home"), "NO_COLOR": "1"}
        checks = (
            ("unreal", ["--help"], 0, "usage: unreal"),
            ("unreal-agent", ["serve", "-h"], 1, "Usage of unreal-agent serve"),
            ("unreal-agent-runner", ["-h"], 0, "unreal-agent-runner [options]"),
            ("unreal-agent-auth", ["methods"], 0, "openai-codex"),
        )
        for name, args, expected_code, expected_text in checks:
            binary = directory / name
            result = subprocess.run([str(binary), *args], cwd=directory, env=smoke_env,
                                    capture_output=True, text=True, timeout=20)
            if result.returncode != expected_code or expected_text not in result.stdout + result.stderr:
                raise ValueError(f"{name}: non-inference CLI smoke failed")
            info = command(["go", "version", "-m", str(binary)], root, env)
            if ("CGO_ENABLED=1" not in info or "vcs.revision=" + metadata["revision"] not in info
                    or "GOOS=linux" not in info or "GOARCH=amd64" not in info):
                raise ValueError(f"{name}: embedded build metadata mismatch")
        for name in ("unreal", "unreal-agent", "unreal-agent-runner"):
            info = command(["go", "version", "-m", str(directory / name)], root, env)
            if "github.com/tree-sitter/go-tree-sitter" not in info:
                raise ValueError(f"{name}: AST parser dependency missing")
        test_env = dict(env, UNREAL_SNAPSHOT_BIN_DIR=str(directory))
        subprocess.run(["go", "test", "./cmd/unreal-agent", "-run", "^TestSnapshotPackaged", "-count=1", "-v"],
                       cwd=root, env=test_env, check=True, timeout=120)


def build(root, executable):
    env = dict(os.environ, CGO_ENABLED="1", GOAMD64="v1", GIT_OPTIONAL_LOCKS="0")
    preflight(root, executable, env)
    output = reserve_output(root)
    env["UNREAL_SNAPSHOT_ROOT"] = str(output)
    config = str(configuration(root, output))
    print(f"snapshot output: {output}", flush=True)
    subprocess.run([executable, "check", config], cwd=root, env=env, check=True)
    subprocess.run([executable, "release", "--config", config, "--snapshot", "--skip=publish"],
                   cwd=root, env=env, check=True)
    archive, metadata = inspect_archive(output / "artifacts")
    smoke_archive(archive, metadata, root, env)
    print(f"snapshot verified: {archive}", flush=True)
    print("checksum, archive contents, CGO/AST build metadata, and non-inference CLI smoke: PASS")
    return output


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=("build", "prepare"))
    parser.add_argument("--goreleaser", default="goreleaser")
    parser.add_argument("--snapshot")
    parser.add_argument("--version")
    parser.add_argument("--commit")
    parser.add_argument("--date")
    args = parser.parse_args()
    try:
        if args.mode == "prepare":
            env = dict(os.environ, UNREAL_SNAPSHOT_IS_SNAPSHOT=args.snapshot or "",
                       UNREAL_SNAPSHOT_VERSION=args.version or "",
                       UNREAL_SNAPSHOT_COMMIT=args.commit or "",
                       UNREAL_SNAPSHOT_DATE=args.date or "")
            prepare(ROOT, env)
        else:
            build(ROOT, args.goreleaser)
    except (OSError, ValueError, subprocess.SubprocessError, tarfile.TarError) as error:
        print(f"snapshot failed: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())

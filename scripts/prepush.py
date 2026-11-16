#!/usr/bin/env python3
"""Run local pre-commit checks, selecting Go tests from HEAD and optional BASE."""

import argparse
from collections import defaultdict
import json
import os
from pathlib import Path
import shlex
import shutil
import subprocess
import sys


def changed_paths(root, base=""):
    commands = [
        ["git", "diff", "--name-only", "--no-renames", "-z", "HEAD", "--"],
        ["git", "ls-files", "--others", "--exclude-standard", "-z", "--"],
    ]
    if base:
        commands.append([
            "git", "diff", "--name-only", "--no-renames", "-z", f"{base}...HEAD", "--",
        ])
    paths = set()
    for command in commands:
        output = subprocess.check_output(command, cwd=root)
        paths.update(os.fsdecode(path) for path in output.split(b"\0") if path)
    return paths


def json_objects(output):
    """go list emits consecutive JSON objects rather than a JSON array."""
    decoder = json.JSONDecoder()
    offset = 0
    while offset < len(output):
        if output[offset].isspace():
            offset += 1
            continue
        value, offset = decoder.raw_decode(output, offset)
        yield value


def affected_packages(root, paths, packages):
    """Return local packages reached through reverse production/test imports."""
    owners = {}
    for package in packages:
        module = package.get("Module", {})
        directory = Path(package.get("Dir", "/"))
        if module.get("Main") and directory.is_relative_to(root):
            relative = directory.relative_to(root).as_posix()
            expected = module["Path"] + ("/" + relative if relative != "." else "")
            if package["ImportPath"] == expected:
                owners[directory] = expected

    # Merge internal/external test variants into the owning package before
    # traversing, so test-only import edges also propagate to its importers.
    names = {
        package["ImportPath"]: owners.get(
            Path(package.get("Dir", "/")), package["ImportPath"].split(" [", 1)[0],
        ) for package in packages
    }
    reverse = defaultdict(set)
    for package in packages:
        for imported in package.get("Imports", []):
            reverse[names.get(imported, imported)].add(names[package["ImportPath"]])

    changed_dirs = {root / Path(path).parent for path in paths}
    if not changed_dirs <= owners.keys():
        # A deleted package, or sources excluded by build tags, cannot be mapped.
        return ["./..."]
    reached = {owners[directory] for directory in changed_dirs}
    pending = list(reached)
    while pending:
        for dependent in reverse[pending.pop()] - reached:
            reached.add(dependent)
            pending.append(dependent)
    return sorted(reached & set(owners.values()))


def go_tests(root, paths):
    if not paths:
        return []
    # Non-Go inputs may be read from anywhere by tests or go:embed. Without a
    # reliable dependency graph for them, run the full suite (including testdata).
    if any(Path(path).suffix != ".go" or "testdata" in Path(path).parts for path in paths):
        return ["./..."]
    output = subprocess.check_output([
        "go", "list", "-p", "1", "-deps", "-test",
        "-json=Dir,ImportPath,ForTest,Module,Imports", "./...",
    ], cwd=root, text=True)
    # go list reports symlink-resolved directories (macOS /tmp and /var).
    return affected_packages(root.resolve(), paths, list(json_objects(output)))


def run(command, root):
    print("+ " + shlex.join(command), flush=True)
    subprocess.run(command, cwd=root, check=True)


def prepush(root, base=""):
    paths = changed_paths(root, base)
    harbor_changed = any(path.startswith("benchmarks/harbor/") for path in paths)
    if harbor_changed and shutil.which("uv") is None:
        raise RuntimeError(
            "benchmarks/harbor changed, but uv is not on PATH. "
            "Install the version pinned in .github/workflows/ci.yml "
            "(https://docs.astral.sh/uv/getting-started/installation/)."
        )
    unformatted = subprocess.check_output([
        "gofmt", "-l", "cmd", "harness", "internal",
    ], cwd=root, text=True)
    if unformatted:
        raise RuntimeError("gofmt is required:\n" + unformatted)
    run(["go", "vet", "-p", "1", "./..."], root)
    packages = go_tests(root, paths)
    if packages:
        run(["go", "test", "-p", "1", *packages], root)
    else:
        print("No changed files: no affected Go packages.", flush=True)
    run(["python3", "-m", "unittest", "discover", "-p", "test_*.py"], root / "scripts")
    if harbor_changed:
        run(["make", "-C", "benchmarks/harbor", "check", "test"], root)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base", default=os.environ.get("BASE", ""))
    args = parser.parse_args()
    try:
        prepush(Path(__file__).resolve().parent.parent, args.base)
    except (OSError, RuntimeError, subprocess.CalledProcessError) as error:
        print(f"prepush: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())

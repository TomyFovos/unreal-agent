#!/usr/bin/env python3
"""Package an existing CGO Linux amd64 snapshot; never build or publish it."""

import argparse
import gzip
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tarfile
import tempfile

import snapshot


BASELINE = "6284e49e70fd603902133bc194ae1ca81c777edd"
KIT_ROOT = "unreal-agent-asp-pilot-" + BASELINE[:7]
ARCHIVE_NAME = KIT_ROOT + ".tar.gz"
FILES = (*snapshot.BINARIES, "LICENSE", "SNAPSHOT.json", "KIT.json", "README.md", "install.sh", "doctor.sh")


def digest(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def binary_requirements(directory, revision):
    versions = set()
    for name in snapshot.BINARIES:
        binary = directory / name
        with binary.open("rb") as stream:
            header = stream.read(20)
        if len(header) != 20 or header[:7] != b"\x7fELF\x02\x01\x01" or header[18:20] != b"\x3e\x00":
            raise ValueError("snapshot requires ELF64 Linux amd64 executables")
        info = subprocess.check_output(["go", "version", "-m", str(binary)], text=True)
        for setting in ("CGO_ENABLED=1", "GOAMD64=v1", "GOOS=linux", "GOARCH=amd64", "vcs.revision=" + revision):
            if setting not in info:
                raise ValueError(f"{name}: embedded build metadata mismatch")
        if name != "unreal-agent-auth" and "github.com/tree-sitter/go-tree-sitter" not in info:
            raise ValueError("CGO AST dependency missing")
        version_info = subprocess.check_output(["readelf", "--version-info", str(binary)], text=True)
        versions.update(tuple(map(int, match.split("."))) for match in re.findall(r"GLIBC_([0-9]+(?:\.[0-9]+)+)", version_info))
        dynamic = subprocess.check_output(["readelf", "--dynamic", str(binary)], text=True)
        libraries = set(re.findall(r"\(NEEDED\).*?\[([^\]]+)\]", dynamic))
        if not libraries <= {"libc.so.6"}:
            raise ValueError("snapshot has additional dynamic library requirements; inspect before distribution")
    if not versions:
        raise ValueError("expected CGO glibc dependency")
    minimum = max(versions)
    if len(minimum) != 2:
        raise ValueError("glibc patch-level requirement needs explicit compatibility review")
    return ".".join(map(str, minimum))


def assemble(source, output, templates):
    archive, metadata = snapshot.inspect_archive(source.parent)
    if archive != source or metadata.get("revision") != BASELINE:
        raise ValueError("use the verified 6284e49 baseline snapshot")
    source_digest = digest(archive)
    package = output / KIT_ROOT
    package.mkdir(mode=0o700)
    with tarfile.open(archive, "r:gz") as bundle:
        for name in (*snapshot.BINARIES, "LICENSE", "SNAPSHOT.json"):
            with bundle.extractfile(name) as stream, (package / name).open("xb") as target:
                # The inspected source contains exactly seven regular members.
                while chunk := stream.read(1024 * 1024):
                    target.write(chunk)
    minimum = binary_requirements(package, BASELINE)
    values = {"MIN_GLIBC": minimum, "GLIBC_MAJOR": minimum.split(".")[0], "GLIBC_MINOR": minimum.split(".")[1],
              "REVISION": BASELINE, "REVISION_SHORT": BASELINE[:7], "ARCHIVE": ARCHIVE_NAME, "KIT_ROOT": KIT_ROOT}
    for name in ("install.sh", "doctor.sh", "README.md"):
        text = (templates / name).read_text(encoding="utf-8")
        for key, value in values.items():
            text = text.replace("@" + key + "@", value)
        if re.search(r"@[A-Z_]+@", text):
            raise ValueError("unexpanded kit template")
        (package / name).write_text(text, encoding="utf-8")
    manifest = {"format_version": 1, "kind": "asp-pilot-kit", "baseline": BASELINE,
                "source_archive_sha256": source_digest, "minimum_glibc": minimum,
                "target": "linux/amd64", "goamd64": "v1", "cgo_enabled": True, "formal_release": False}
    (package / "KIT.json").write_text(json.dumps(manifest, indent=2) + "\n", encoding="ascii")
    for name in FILES:
        (package / name).chmod(0o700 if name in (*snapshot.BINARIES, "install.sh", "doctor.sh") else 0o600)
    sums = "".join(f"{digest(package / name)}  {name}\n" for name in FILES)
    (package / "SHA256SUMS").write_text(sums, encoding="ascii")
    (package / "SHA256SUMS").chmod(0o600)
    target = output / ARCHIVE_NAME
    with target.open("xb") as raw, gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=0) as compressed:
        with tarfile.open(fileobj=compressed, mode="w", format=tarfile.USTAR_FORMAT) as bundle:
            for name in (*FILES, "SHA256SUMS"):
                info = bundle.gettarinfo(str(package / name), KIT_ROOT + "/" + name)
                info.uid = info.gid = 0
                info.uname = info.gname = ""
                info.mtime = 0
                with (package / name).open("rb") as stream:
                    bundle.addfile(info, stream)
    target.chmod(0o600)
    checksum = output / (ARCHIVE_NAME + ".sha256")
    checksum.write_text(f"{digest(target)}  {ARCHIVE_NAME}\n", encoding="ascii")
    checksum.chmod(0o600)
    return target


def private_output(root):
    parent = root
    for part in ("dist", "pilot-kits"):
        parent = parent / part
        if parent.is_symlink():
            raise ValueError("kit output parent must not be a symlink")
        parent.mkdir(mode=0o700, exist_ok=True)
    return Path(tempfile.mkdtemp(prefix="kit-", dir=parent))


def copy_delivery(archive, destination):
    # Reserve a new child directory so no previous Windows files are overwritten.
    destination = destination.absolute()
    for parent in (*reversed(destination.parents), destination):
        if parent.is_symlink():
            raise ValueError("delivery parent must not be a symlink")
    destination.mkdir(parents=True, exist_ok=True)
    delivery = Path(tempfile.mkdtemp(prefix=KIT_ROOT + "-", dir=destination))
    for source in (archive, archive.with_name(archive.name + ".sha256")):
        target = delivery / source.name
        with source.open("rb") as reader, target.open("xb") as writer:
            while chunk := reader.read(1024 * 1024):
                writer.write(chunk)
            writer.flush()
            os.fsync(writer.fileno())
        if digest(source) != digest(target):
            raise ValueError("delivery copy checksum mismatch")
    return delivery


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--snapshot", required=True, type=Path)
    parser.add_argument("--copy-to", type=Path, help="optional delivery parent; always reserves a new child directory")
    args = parser.parse_args()
    try:
        output = private_output(snapshot.ROOT)
        archive = assemble(args.snapshot.resolve(), output, Path(__file__).with_name("pilot"))
        print(f"kit: {archive}")
        print(f"SHA256: {digest(archive)}")
        if args.copy_to:
            print(f"delivery: {copy_delivery(archive, args.copy_to)}")
    except (OSError, ValueError, subprocess.SubprocessError, tarfile.TarError) as error:
        print(f"pilot kit failed: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())

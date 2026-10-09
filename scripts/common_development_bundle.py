#!/usr/bin/env python3
"""One CGO product archive; existing native discovery chooses the provider."""

import argparse
from pathlib import Path
import subprocess
import sys
import tarfile

import common_development as common
import pilot_kit
import snapshot


def assemble(source, output, revision):
    scripts = Path(__file__).parent
    templates = scripts / "pilot"
    return pilot_kit.assemble(source, output, templates, revision=revision, sandbox="",
        kit_name=f"unreal-agent-linux-amd64-{revision[:7]}", kind="common-development-kit",
        readme=(templates / "common-README.md").read_text(encoding="utf-8"),
        extra_files={name: (templates / name if name.endswith(".sh") else scripts / name).read_bytes()
                     for name in common.EXTRAS})


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--snapshot", required=True, type=Path)
    parser.add_argument("--revision", required=True)
    parser.add_argument("--copy-to", type=Path)
    args = parser.parse_args()
    try:
        archive = assemble(args.snapshot.resolve(), pilot_kit.private_output(snapshot.ROOT), args.revision)
        print(f"kit: {archive}\nSHA256: {pilot_kit.digest(archive)}")
        if args.copy_to:
            print("delivery: " + str(pilot_kit.copy_delivery(archive, args.copy_to,
                                    prefix=f"common-{args.revision[:7]}-")))
    except (OSError, ValueError, subprocess.SubprocessError, tarfile.TarError):
        print("common kit failed: preserve old archives; inspect isolated snapshot checks", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())

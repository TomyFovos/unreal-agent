#!/usr/bin/env python3
"""Package only asp's updated snapshot and explicit OAuth profile; never publish."""

import argparse
from pathlib import Path
import subprocess
import sys
import tarfile

import pilot_kit
import snapshot


def assemble(source, output, revision):
    scripts = Path(__file__).parent
    templates = scripts / "pilot"
    return pilot_kit.assemble(source, output, templates, revision=revision, extra_files={
        "asp_oauth.py": (scripts / "asp_oauth.py").read_bytes(),
        "pilot_provider.py": (scripts / "pilot_provider.py").read_bytes(),
        "activate.sh": (templates / "asp-activate.sh").read_bytes(),
    }, readme=(templates / "asp-oauth-README.md").read_text(encoding="utf-8"))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--snapshot", required=True, type=Path)
    parser.add_argument("--revision", required=True)
    parser.add_argument("--copy-to", type=Path)
    args = parser.parse_args()
    try:
        output = pilot_kit.private_output(snapshot.ROOT)
        archive = assemble(args.snapshot.resolve(), output, args.revision)
        print(f"kit: {archive}\nSHA256: {pilot_kit.digest(archive)}")
        if args.copy_to:
            print(f"delivery: {pilot_kit.copy_delivery(archive, args.copy_to, prefix=archive.name[:-7] + '-')}")
    except (OSError, ValueError, subprocess.SubprocessError, tarfile.TarError):
        print("asp OAuth kit failed; preserve existing files and inspect snapshot provenance/checksums", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())

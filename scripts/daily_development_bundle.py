#!/usr/bin/env python3
"""Package daily Git-workspace launchers using the existing CGO snapshot kit."""

import argparse
from pathlib import Path
import subprocess
import sys
import tarfile

import daily_development as daily
import pilot_kit
import snapshot


def assemble(source, output, revision, sandbox):
    if sandbox not in ("asp", "devlore"):
        raise ValueError("invalid Sandbox")
    scripts = Path(__file__).parent
    templates = scripts / "pilot"
    readme = (templates / "daily-README.md").read_text(encoding="utf-8")
    values = {"SANDBOX": sandbox, "MODEL": "gpt-6.1-sol" if sandbox == "asp" else "opus",
              "OPT_IN": "" if sandbox == "asp" else " --policy-audited --trust-managed-policy"}
    for key, value in values.items():
        readme = readme.replace("@" + key + "@", value)
    return pilot_kit.assemble(source, output, templates, revision=revision, sandbox=sandbox,
        kit_name=f"unreal-agent-{sandbox}-development-{revision[:7]}", kind="daily-development-kit", readme=readme,
        extra_files={name: (templates / name if name.endswith(".sh") else scripts / name).read_bytes() for name in daily.EXTRAS})


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--snapshot", type=Path, required=True)
    parser.add_argument("--revision", required=True)
    parser.add_argument("--sandbox", choices=("asp", "devlore"), required=True)
    parser.add_argument("--copy-to", type=Path)
    args = parser.parse_args()
    try:
        archive = assemble(args.snapshot.resolve(), pilot_kit.private_output(snapshot.ROOT), args.revision, args.sandbox)
        print(f"kit: {archive}\nSHA256: {pilot_kit.digest(archive)}")
        if args.copy_to:
            print("delivery: " + str(pilot_kit.copy_delivery(archive, args.copy_to, prefix=archive.name[:-7] + "-")))
    except (OSError, ValueError, subprocess.SubprocessError, tarfile.TarError):
        print("daily kit failed: preserve existing archives; inspect snapshot provenance/checksums", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())

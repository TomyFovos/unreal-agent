#!/usr/bin/env python3
"""Package fresh permission-capable Linux amd64 binaries for asp/devlore only."""

import argparse
from pathlib import Path
import subprocess
import sys
import tarfile

import pilot_kit
import snapshot


def assemble(source, output, revision, sandbox):
    if sandbox not in ("asp", "devlore"):
        raise ValueError("invalid Sandbox")
    scripts = Path(__file__).parent
    templates = scripts / "pilot"
    readme = (templates / "permission-README.md").read_text(encoding="utf-8")
    root = f"unreal-agent-{sandbox}-permissions-{revision[:7]}"
    values = {"SANDBOX": sandbox, "MODEL": "gpt-6.1-sol" if sandbox == "asp" else "opus",
              "OPT_IN": "" if sandbox == "asp" else " --policy-audited --trust-managed-policy"}
    for key, value in values.items():
        readme = readme.replace("@"+key+"@", value)
    return pilot_kit.assemble(source, output, templates, revision=revision, sandbox=sandbox,
        kit_name=root, kind="permission-acceptance-kit", readme=readme, extra_files={
            **{name: (scripts / name).read_bytes() for name in ("asp_oauth.py", "devlore_launcher.py", "permission_acceptance.py", "pilot_provider.py")},
            "gate.sh": (templates / "gate.sh").read_bytes()})


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--snapshot", required=True, type=Path)
    parser.add_argument("--revision", required=True)
    parser.add_argument("--sandbox", choices=("asp", "devlore"), required=True)
    parser.add_argument("--copy-to", type=Path)
    args = parser.parse_args()
    try:
        output = pilot_kit.private_output(snapshot.ROOT)
        archive = assemble(args.snapshot.resolve(), output, args.revision, args.sandbox)
        print(f"kit: {archive}\nSHA256: {pilot_kit.digest(archive)}")
        if args.copy_to:
            print(f"delivery: {pilot_kit.copy_delivery(archive, args.copy_to, prefix=archive.name[:-7]+'-')}")
    except (OSError, ValueError, subprocess.SubprocessError, tarfile.TarError):
        print("permission kit failed: preserve existing kits and inspect snapshot provenance/checksums", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())

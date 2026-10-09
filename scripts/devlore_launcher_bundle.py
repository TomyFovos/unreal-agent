#!/usr/bin/env python3
"""Package only devlore's permanent launcher; existing binaries/auth are reused."""

import argparse
import gzip
import json
from pathlib import Path
import re
import subprocess
import tarfile

import devlore_launcher
import pilot_kit
import snapshot


def build(output, revision):
    if not re.fullmatch(r"[0-9a-f]{40}", revision):
        raise ValueError("expected full launcher revision")
    name = "unreal-agent-devlore-launcher-" + revision[:7]
    package = output / name
    package.mkdir(mode=0o700)
    scripts = Path(__file__).parent
    for filename in ("devlore_launcher.py", "pilot_provider.py"):
        (package / filename).write_bytes((scripts / filename).read_bytes())
    values = {"ROOT": name, "ARCHIVE": name + ".tar.gz", "REVISION": revision}
    for filename, template in (("install.sh", "devlore-install.sh"), ("README.md", "devlore-README.md")):
        text = (scripts / "pilot" / template).read_text()
        for key, value in values.items():
            text = text.replace("@" + key + "@", value)
        (package / filename).write_text(text)
    (package / "KIT.json").write_text(json.dumps({"version": 1, "kind": "devlore-launcher-kit",
        "launcher_revision": revision, "binary_revision": pilot_kit.BASELINE}, indent=2) + "\n")
    for filename in devlore_launcher.MEMBERS:
        (package / filename).chmod(0o700 if filename == "install.sh" else 0o600)
    (package / "SHA256SUMS").write_text("".join(f"{pilot_kit.digest(package / filename)}  {filename}\n" for filename in devlore_launcher.MEMBERS))
    (package / "SHA256SUMS").chmod(0o600)
    archive = output / (name + ".tar.gz")
    with archive.open("xb") as raw, gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=0) as compressed:
        with tarfile.open(fileobj=compressed, mode="w", format=tarfile.USTAR_FORMAT) as bundle:
            for filename in (*devlore_launcher.MEMBERS, "SHA256SUMS"):
                info = bundle.gettarinfo(str(package / filename), name + "/" + filename)
                info.uid = info.gid = info.mtime = 0
                info.uname = info.gname = ""
                with (package / filename).open("rb") as stream:
                    bundle.addfile(info, stream)
    archive.chmod(0o600)
    archive.with_name(archive.name + ".sha256").write_text(f"{pilot_kit.digest(archive)}  {archive.name}\n")
    archive.with_name(archive.name + ".sha256").chmod(0o600)
    return archive


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--copy-to", type=Path)
    args = parser.parse_args()
    revision = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=snapshot.ROOT, text=True).strip()
    archive = build(pilot_kit.private_output(snapshot.ROOT), revision)
    print(f"kit: {archive}\nSHA256: {pilot_kit.digest(archive)}")
    if args.copy_to:
        print("delivery: " + str(pilot_kit.copy_delivery(archive, args.copy_to, prefix=archive.name[:-7] + "-")))


if __name__ == "__main__":
    main()

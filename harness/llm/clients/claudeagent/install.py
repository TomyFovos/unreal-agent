"""Install the pinned Linux runtime for this Claude Code adapter.

No npm lifecycle scripts are run. The platform package is the official npm
artifact, checked against its published SHA-512 integrity before extraction.
"""

import argparse
import base64
import hashlib
import io
import tarfile
import urllib.request
from pathlib import Path

NODE_VERSION = "22.16.0"
CLAUDE_VERSION = "2.1.322"
# https://nodejs.org/dist/v22.16.0/SHASUMS256.txt
NODE_SHA256 = {
    "amd64": "f4cb75bb036f0d0eddf6b79d9596df1aaab9ddccd6a20bf489be5abe9467e84e",
    "arm64": "eab80cb88f8fda1e65f5e8d0420c9809bdb320b03fd34976ab7161b6e703b910",
}
# https://registry.npmjs.org/@anthropic-ai/claude-code-linux-{x64,arm64}/2.1.322
CLAUDE_INTEGRITY = {
    "amd64": "0DoaNAXLWCYYIGachWBCkTUsS8dOOgvk4R2fFKdrLejO9XlRTwaWUMD9ONdDmv0csdpwxj4qLUN6zWMaG2Sz6g==",
    "arm64": "YYNp1op87WWcBEE6jC1neOjMDPdn4Iy+kUHyFcI1GsLl4IwH7EjZlwVKcpsZqVbE0IXEFaOldmyOHC1y3j18Ag==",
}


def verified_download(url, algorithm, expected):
    with urllib.request.urlopen(url) as response:
        data = response.read()
    digest = hashlib.new(algorithm, data).digest()
    if digest != expected:
        raise ValueError("Runtime artifact integrity mismatch")
    return data


def install(destination, arch):
    destination.mkdir(parents=True, exist_ok=True)
    platform = {"amd64": "x64", "arm64": "arm64"}[arch]
    node = f"node-v{NODE_VERSION}-linux-{platform}"
    data = verified_download(
        f"https://nodejs.org/dist/v{NODE_VERSION}/{node}.tar.xz",
        "sha256",
        bytes.fromhex(NODE_SHA256[arch]),
    )
    with tarfile.open(fileobj=io.BytesIO(data), mode="r:xz") as archive:
        (destination / "node").write_bytes(
            archive.extractfile(f"{node}/bin/node").read()
        )
    package = f"claude-code-linux-{platform}"
    data = verified_download(
        f"https://registry.npmjs.org/@anthropic-ai/{package}/-/"
        f"{package}-{CLAUDE_VERSION}.tgz",
        "sha512",
        base64.b64decode(CLAUDE_INTEGRITY[arch]),
    )
    with tarfile.open(fileobj=io.BytesIO(data), mode="r:gz") as archive:
        (destination / "claude").write_bytes(
            archive.extractfile("package/claude").read()
        )
    for name in ("node", "claude"):
        (destination / name).chmod(0o755)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--destination", type=Path, required=True)
    parser.add_argument("--arch", choices=NODE_SHA256, required=True)
    args = parser.parse_args()
    install(args.destination, args.arch)

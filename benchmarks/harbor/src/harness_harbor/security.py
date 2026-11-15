"""Sanitize only exported artifacts; keys and runtime state stay private."""

import argparse
import base64
import json
import re
import stat
from pathlib import Path
from urllib.parse import quote

CREDENTIAL = re.compile(
    r"(?i)\b(?:sk-[A-Za-z0-9_-]{8,}|eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+)"
    r"|\bbearer\s+[^\s\"',}]+"
    r"|\b(?:[A-Z_]*(?:API[_-]?KEY|AUTH[_-]?TOKEN|ACCESS[_-]?TOKEN|REFRESH[_-]?TOKEN|CLIENT[_-]?SECRET|TOKEN|SECRET)"
    r"|authorization|password)\b[\"']?\s*[:=]\s*[\"']?(?:bearer\s+)?[^\s\"',}]+"
)
PRIVATE_FIELDS = {
    "token",
    "secret",
    "oauth_token",
    "auth_token",
    "client_secret",
    "password",
    "private_key",
    "apikey",
    "raw",
    "auth",
    "authmethod",
    "apikeys",
    "apikeysource",
    "api_key",
    "access_token",
    "refresh_token",
    "id_token",
    "authorization",
    "signature",
    "encrypted_content",
    "encryptedcontent",
    "account_id",
    "accountid",
    "organization",
    "email",
}


def read_key(path: str) -> str:
    try:
        source = Path(path)
        if not source.is_file() or stat.S_IMODE(source.stat().st_mode) != 0o600:
            raise ValueError("Key file must be a regular file with mode 0600")
        key = source.read_text().strip()
    except OSError:
        raise ValueError("Key file could not be read") from None
    if not key or any(c in key for c in "\0\r\n"):
        raise ValueError("Key file is empty or invalid")
    return key


class Redactor:
    def __init__(self, secrets=()):
        self.secrets = set()
        for value in secrets:
            if value:
                self.secrets.update(
                    (
                        value,
                        json.dumps(value)[1:-1],
                        quote(value, safe=""),
                        base64.b64encode(value.encode()).decode(),
                    )
                )

    def text(self, value):
        for secret in sorted(self.secrets, key=len, reverse=True):
            value = value.replace(secret, "[redacted]")
        return CREDENTIAL.sub("[redacted]", value)

    def clean(self, value):
        if isinstance(value, str):
            return self.text(value)
        if isinstance(value, dict):
            return {
                self.text(k): self.clean(v)
                for k, v in value.items()
                if k.lower() not in PRIVATE_FIELDS
            }
        if isinstance(value, list):
            return [self.clean(v) for v in value]
        return value

    def jsonl(self, text):
        return "".join(
            json.dumps(self.clean(json.loads(line)), ensure_ascii=False) + "\n"
            for line in text.splitlines()
            if line.strip()
        )


def scan(paths, secrets):
    redactor = Redactor(secrets)
    found = []
    for root in paths:
        files = sorted(root.rglob("*")) if root.is_dir() else [root]
        for path in files:
            if path.is_file():
                data = path.read_bytes()
                if any(
                    s.encode() in data for s in redactor.secrets
                ) or CREDENTIAL.search(data.decode("utf-8", errors="ignore")):
                    found.append(str(path))
    return found


if __name__ == "__main__":
    parser = argparse.ArgumentParser(
        description="Scan before publication; print paths only, never secret values"
    )
    parser.add_argument("paths", type=Path, nargs="+")
    parser.add_argument("--key-file", action="append", default=[])
    args = parser.parse_args()
    matches = scan(args.paths, [read_key(p) for p in args.key_file])
    print(
        "\n".join(matches)
        if matches
        else "No credential matches found; review task data before publication."
    )
    raise SystemExit(bool(matches))

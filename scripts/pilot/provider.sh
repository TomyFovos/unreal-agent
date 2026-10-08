#!/usr/bin/env bash
set -euo pipefail
directory=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
command -v python3 >/dev/null || { printf 'FAIL: python3 required; no packages installed\n' >&2; exit 1; }
[[ ! -L "$directory/SHA256SUMS" && ! -L "$directory/pilot_provider.py" ]] || { printf 'FAIL: profile symlink refused\n' >&2; exit 1; }
(cd -- "$directory" && sha256sum --check --status SHA256SUMS) || { printf 'FAIL: profile checksum mismatch\n' >&2; exit 1; }
exec python3 "$directory/pilot_provider.py" @SANDBOX@ "$@"

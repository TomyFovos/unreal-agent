#!/bin/bash
set -euo pipefail
umask 077
directory=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }
files=(devlore_launcher.py pilot_provider.py install.sh README.md KIT.json)
[[ -f "$directory/SHA256SUMS" && ! -L "$directory/SHA256SUMS" ]] || fail 'checksum manifest missing/redirected'
[[ $(stat -c %s -- "$directory/SHA256SUMS") -le 4096 ]] || fail 'checksum manifest too large'
for name in "${files[@]}"; do
    [[ -f "$directory/$name" && ! -L "$directory/$name" ]] || fail 'kit member missing/redirected'
done
expected=$(cat -- "$directory/SHA256SUMS")
actual=$(cd -- "$directory" && sha256sum -- "${files[@]}")
[[ "$expected" == "$actual" ]] || fail 'kit checksum/inventory mismatch'
command -v python3 >/dev/null || fail 'python3 required; no packages installed'
exec python3 "$directory/devlore_launcher.py" install --package "$directory" "$@"

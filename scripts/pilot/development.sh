#!/usr/bin/env bash
set -euo pipefail
umask 077
KIT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
command -v python3 >/dev/null || { printf 'Python 3 required; no packages installed\n' >&2; exit 1; }
exec python3 -B "$KIT_DIR/common_development.py" install "$@"

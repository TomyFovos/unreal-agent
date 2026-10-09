#!/usr/bin/env bash
set -euo pipefail
umask 077
DAILY_PACKAGE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
exec python3 -B "$DAILY_PACKAGE/daily_development.py" "$@"

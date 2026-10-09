#!/usr/bin/env bash
set -euo pipefail
umask 077
GATE_PACKAGE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
exec python3 -B "$GATE_PACKAGE/permission_acceptance.py" "$@"

#!/usr/bin/env bash
# Only inspect the kit and run help/methods in a disposable, empty HOME.
set -euo pipefail
umask 077
KIT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
FILES=(unreal unreal-agent unreal-agent-runner unreal-agent-auth LICENSE SNAPSHOT.json KIT.json README.md install.sh doctor.sh @EXTRA_FILES@)
fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }
pass() { printf 'PASS: %s\n' "$1"; }
[[ $# -eq 0 || ( $# -eq 1 && $1 == --preflight ) ]] || fail 'usage: doctor.sh [--preflight]'
temporary=$(mktemp -d "${TMPDIR:-/tmp}/unreal-pilot-doctor.XXXXXXXX")
trap 'rm -rf -- "$temporary"' EXIT
mkdir -- "$temporary/home"
smoke() {
    local name=$1 code=$2 marker=$3 status=0
    shift 3
    (cd -- "$temporary" && timeout 15s env -i PATH="$PATH" HOME="$temporary/home" NO_COLOR=1 LC_ALL=C \
        "$KIT_DIR/$name" "$@") >"$temporary/output" 2>&1 || status=$?
    [[ $status -eq $code ]] && grep -Fq -- "$marker" "$temporary/output" || fail "$name: non-inference CLI check failed (output withheld)"
    pass "$name: non-inference CLI"
}
smoke unreal 0 'usage: unreal' --help
smoke unreal-agent 1 'Usage of unreal-agent serve' serve -h
smoke unreal-agent-runner 0 'unreal-agent-runner [options]' -h
smoke unreal-agent-auth 0 'openai-codex' methods
# Inspect only file existence/readability. Never read/import authentication.
auth_file=${OPENAI_CODEX_AUTH_FILE:-${CODEX_HOME:-$HOME/.codex}/auth.json}
if [[ -f "$auth_file" && ! -L "$auth_file" && -r "$auth_file" ]]; then
    printf 'INFO: existing External Codex auth path readable; contents/validity not checked\n'
else
    printf 'WARN: External Codex auth path missing/unreadable; keep existing Codex authentication ownership\n'
fi
if command -v codex >/dev/null 2>&1; then
    printf 'INFO: existing Codex CLI available; not executed or modified\n'
else
    printf 'WARN: Codex CLI not on PATH; no installation/authentication attempted\n'
fi
printf 'INFO: network/API connectivity NOT probed; no inference or configuration changes\n'
printf 'INFO: CGO/tree-sitter retained; no Host started, permissions granted, or Session Store changed\n'

#!/usr/bin/env bash
# Only inspect the kit and run help/methods in a disposable, empty HOME.
set -euo pipefail
umask 077
KIT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
FILES=(unreal unreal-agent unreal-agent-runner unreal-agent-auth LICENSE SNAPSHOT.json KIT.json README.md install.sh doctor.sh)
fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }
pass() { printf 'PASS: %s\n' "$1"; }
[[ $# -eq 0 || ( $# -eq 1 && $1 == --preflight ) ]] || fail 'usage: doctor.sh [--preflight]'
for tool in uname getconf ldd sha256sum stat od tr env timeout mktemp rm mkdir cat grep cmp cp chmod mv rmdir; do
    command -v "$tool" >/dev/null 2>&1 || fail "required command unavailable: $tool (no packages installed)"
done
[[ -f "$KIT_DIR/SHA256SUMS" && ! -L "$KIT_DIR/SHA256SUMS" ]] || fail 'checksum manifest missing or symlink'
[[ $(stat -c %s -- "$KIT_DIR/SHA256SUMS") -le 4096 ]] || fail 'checksum manifest too large'
for name in "${FILES[@]}"; do
    [[ -f "$KIT_DIR/$name" && ! -L "$KIT_DIR/$name" ]] || fail "kit member missing or symlink: $name"
done
expected=$(cat -- "$KIT_DIR/SHA256SUMS")
actual=$(cd -- "$KIT_DIR" && sha256sum -- "${FILES[@]}")
[[ "$actual" == "$expected" ]] || fail 'kit checksum mismatch'
pass 'all kit checksums'
[[ $(uname -s) == Linux ]] || fail 'Linux required'
[[ $(uname -m) == x86_64 ]] || fail 'amd64 / x86_64 required'
libc=$(getconf GNU_LIBC_VERSION 2>/dev/null) || fail 'glibc required; musl is unsupported'
[[ "$libc" =~ ^glibc\ ([0-9]+)\.([0-9]+)$ ]] || fail 'unrecognized glibc version'
major=${BASH_REMATCH[1]}; minor=${BASH_REMATCH[2]}
[[ ${#major} -le 3 && ${#minor} -le 3 ]] || fail 'invalid glibc version'
(( 10#$major > @GLIBC_MAJOR@ || (10#$major == @GLIBC_MAJOR@ && 10#$minor >= @GLIBC_MINOR@) )) || fail 'glibc @MIN_GLIBC@ or newer required'
pass "Linux amd64; $libc (required >= @MIN_GLIBC@)"
for name in unreal unreal-agent unreal-agent-runner unreal-agent-auth; do
    [[ -x "$KIT_DIR/$name" ]] || fail "binary not executable: $name"
    header=$(od -An -v -tx1 -N20 -- "$KIT_DIR/$name" | tr -d ' \n')
    [[ ${#header} -eq 40 && ${header:0:14} == 7f454c46020101 && ${header:36:4} == 3e00 ]] || fail "not ELF64 little-endian amd64: $name"
    status=0
    dependencies=$(env -i PATH="$PATH" LC_ALL=C ldd "$KIT_DIR/$name" 2>&1) || status=$?
    if [[ $name == unreal-agent-auth && ( "$dependencies" == *'not a dynamic executable'* || "$dependencies" == *'statically linked'* ) ]]; then
        pass "$name: static executable"
    else
        [[ $status -eq 0 && "$dependencies" != *'not found'* ]] || fail "$name: dynamic library/loader check failed"
        pass "$name: dynamic libraries available"
    fi
done
if [[ ${1:-} == --preflight ]]; then
    exit 0
fi
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

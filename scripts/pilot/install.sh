#!/usr/bin/env bash
# Install only this verified kit into a private, content-addressed directory.
set -euo pipefail
umask 077
[[ $# -eq 0 ]] || { printf 'usage: install.sh\n' >&2; exit 1; }
KIT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }
# Check the doctor itself before invoking it; the doctor checks the full kit.
[[ -f "$KIT_DIR/doctor.sh" && ! -L "$KIT_DIR/doctor.sh" && -f "$KIT_DIR/SHA256SUMS" && ! -L "$KIT_DIR/SHA256SUMS" ]] || fail 'doctor/checksum missing or symlink'
[[ $(stat -c %s -- "$KIT_DIR/SHA256SUMS") -le 4096 ]] || fail 'checksum manifest too large'
doctor_sum=$(sha256sum -- "$KIT_DIR/doctor.sh")
doctor_sum=${doctor_sum%% *}
grep -Fxq -- "$doctor_sum  doctor.sh" "$KIT_DIR/SHA256SUMS" || fail 'doctor checksum mismatch'
bash "$KIT_DIR/doctor.sh" --preflight
[[ ${HOME:-} == /* && -d "$HOME" ]] || fail 'existing absolute user HOME required'
home_dir=$(cd -- "$HOME" && pwd -P)
base=$home_dir
# Preserve existing directories/modes; reject redirects and unsafe parents.
for part in .local opt unreal-agent-pilot asp; do
    base=$base/$part
    [[ ! -L "$base" ]] || fail 'installation parent is a symlink; nothing overwritten'
    if [[ ! -e "$base" ]]; then
        mkdir -m 700 -- "$base" || [[ -d "$base" && ! -L "$base" ]] || fail 'cannot create private installation parent'
    fi
    [[ -d "$base" && -O "$base" && -w "$base" ]] || fail 'installation parent must be a writable user-owned directory'
    mode=$(stat -c %a -- "$base")
    (( (8#$mode & 0022) == 0 )) || fail 'installation parent is group/world writable'
done
lock=$base/.install.lock
mkdir -m 700 -- "$lock" 2>/dev/null || fail 'another install is active or its lock remains; no existing files changed'
stage=
cleanup() {
    [[ -z "$stage" ]] || rm -rf -- "$stage"
    rmdir -- "$lock"
}
trap cleanup EXIT
kit_id=$(sha256sum -- "$KIT_DIR/SHA256SUMS")
kit_id=${kit_id:0:16}
destination=$base/@REVISION_SHORT@-$kit_id
files=(unreal unreal-agent unreal-agent-runner unreal-agent-auth LICENSE SNAPSHOT.json KIT.json README.md install.sh doctor.sh SHA256SUMS)
if [[ -e "$destination" || -L "$destination" ]]; then
    [[ -d "$destination" && ! -L "$destination" && -O "$destination" ]] || fail 'existing install is not a user-owned regular directory'
    [[ -f "$destination/SHA256SUMS" && ! -L "$destination/SHA256SUMS" ]] || fail 'existing install is incomplete; nothing overwritten'
    cmp -s -- "$KIT_DIR/SHA256SUMS" "$destination/SHA256SUMS" || fail 'existing install differs; nothing overwritten'
    for name in "${files[@]}"; do
        [[ -f "$destination/$name" && ! -L "$destination/$name" ]] || fail 'existing install has a missing/redirected member'
    done
    (cd -- "$destination" && sha256sum --check --status SHA256SUMS) || fail 'existing install modified; nothing overwritten'
    printf 'INFO: verified identical installation reused\n'
else
    stage=$(mktemp -d "$base/.install.XXXXXXXX")
    for name in "${files[@]}"; do
        cp -- "$KIT_DIR/$name" "$stage/$name"
        chmod 600 -- "$stage/$name"
    done
    chmod 700 -- "$stage/unreal" "$stage/unreal-agent" "$stage/unreal-agent-runner" "$stage/unreal-agent-auth" "$stage/install.sh" "$stage/doctor.sh"
    bash "$stage/doctor.sh" --preflight
    mv -T -n -- "$stage" "$destination"
    [[ ! -d "$stage" ]] || fail 'installation destination appeared concurrently; nothing overwritten'
    stage=
    printf 'INFO: new private installation published\n'
fi
bash "$destination/doctor.sh"
printf 'INSTALLED: %s\n' "$destination"
printf 'UNCHANGED: Codex CLI, PATH, authentication, runtime configuration, Session Store\n'
printf 'RECOVERY: keep using your original Codex command; this kit starts no Host\n'

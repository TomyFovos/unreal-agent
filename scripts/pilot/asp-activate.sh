#!/usr/bin/env bash
# Explicit shell-only activation. Does not write PATH or a startup/profile file.
if [[ ${BASH_SOURCE[0]} == "$0" ]]; then
    printf 'usage: source activate.sh --model <model> [--instance <name>]\n' >&2
    exit 1
fi
_unreal_asp_activate() {
    local directory output installed result instance=oauth-v1 arg previous=
    directory=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P) || return 1
    command -v python3 >/dev/null || { printf 'FAIL: python3 required; no packages installed\n' >&2; return 1; }
    if declare -F unreal >/dev/null && { [[ ! -f ${_UNREAL_ASP_OAUTH_KIT:-/nonexistent}/asp_oauth.py ]] || [[ $(declare -f unreal) != *'"$_UNREAL_ASP_OAUTH_KIT/asp_oauth.py"'* ]]; }; then
        printf 'FAIL: existing unreal shell function preserved; activate in a fresh shell\n' >&2
        return 1
    fi
    output=$(bash "$directory/install.sh") || { printf '%s\n' "$output" >&2; return 1; }
    printf '%s\n' "$output"
    installed=$(printf '%s\n' "$output" | sed -n 's/^INSTALLED: //p')
    [[ $installed == /* && -f "$installed/asp_oauth.py" && ! -L "$installed/asp_oauth.py" ]] || { printf 'FAIL: installed asp driver unavailable\n' >&2; return 1; }
    PYTHONDONTWRITEBYTECODE=1 python3 "$installed/asp_oauth.py" --kit-directory "$installed" --setup-only "$@" || return 1
    for arg in "$@"; do
        if [[ $previous == --instance ]]; then instance=$arg; fi
        if [[ $arg == --instance=* ]]; then instance=${arg#--instance=}; fi
        previous=$arg
    done
    _UNREAL_ASP_OAUTH_KIT=$installed
    _UNREAL_ASP_OAUTH_INSTANCE=$instance
    unreal() { PYTHONDONTWRITEBYTECODE=1 python3 "$_UNREAL_ASP_OAUTH_KIT/asp_oauth.py" --kit-directory "$_UNREAL_ASP_OAUTH_KIT" --instance "$_UNREAL_ASP_OAUTH_INSTANCE" "$@"; }
    printf 'READY: unreal <session>; /model available; current-shell function only, PATH/default config unchanged\n'
    printf 'RECOVERY: unset -f unreal; original Codex CLI unchanged; keep Pilot history\n'
}
_unreal_asp_activate "$@"
_unreal_asp_result=$?
unset -f _unreal_asp_activate
return "$_unreal_asp_result"

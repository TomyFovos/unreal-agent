#!/bin/sh
# Install a built revision under ~/.local/opt/unreal-agent and switch the
# ~/.local/bin/unreal wrapper to it. Never edits shell startup files.
set -eu

if [ "$#" -lt 1 ] || [ "$#" -gt 2 ] || { [ "$#" -eq 2 ] && [ "$2" != --disable-opencode-go ]; }; then
    printf 'usage: install.sh <commit> [--disable-opencode-go]\n' >&2
    exit 2
fi

rev=$1
repo=$(git rev-parse --show-toplevel)
full=$(git rev-parse --verify "$rev^{commit}")
rev7=$(printf '%s\n' "$full" | cut -c1-7)
opencode_go_enabled=true
if [ "${2-}" = --disable-opencode-go ]; then
    opencode_go_enabled=false
    rev7=$rev7-no-opencode-go
fi
opt="$HOME/.local/opt/unreal-agent"
bin_dir="$HOME/.local/bin"
wrapper="$bin_dir/unreal"
marker='unreal-agent wrapper'

# Refuse before touching anything when another program owns the wrapper path.
if [ -e "$wrapper" ] || [ -L "$wrapper" ]; then
    if ! grep -q "$marker" "$wrapper" 2>/dev/null; then
        printf 'error: %s is owned by another program (it is not the unreal-agent wrapper); nothing changed\n' "$wrapper" >&2
        exit 1
    fi
fi

mkdir -p "$opt"

worktree=
pending=
wrapper_tmp=
cleanup() {
    if [ -n "$worktree" ]; then
        git -C "$repo" worktree remove --force "$worktree" 2>/dev/null || true
        rm -rf "$worktree"
    fi
    if [ -n "$pending" ]; then
        rm -rf "$pending"
    fi
    if [ -n "$wrapper_tmp" ]; then
        rm -f "$wrapper_tmp"
    fi
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM

if [ -e "$opt/$rev7/unreal-agent" ]; then
    printf 'version %s is already installed; switching only\n' "$rev7"
else
    worktree=$(mktemp -d "${TMPDIR:-/tmp}/unreal-agent-build.XXXXXX")
    git -C "$repo" worktree add --detach "$worktree" "$full"
    if [ "$opencode_go_enabled" = false ] && [ ! -f "$worktree/internal/buildfeatures/opencode_go_disabled.go" ]; then
        printf 'error: this revision cannot disable OpenCode Go; use a revision with build feature support\n' >&2
        exit 1
    fi
    pending="$opt/.$rev7.tmp.$$"
    make -C "$worktree" build BIN_DIR="$pending" OPENCODE_GO_ENABLED="$opencode_go_enabled"
    mv "$pending" "$opt/$rev7"
    pending=
fi

previous=$(readlink "$opt/current" 2>/dev/null || true)
ln -sfn "$rev7" "$opt/current"

# When the installed version was already current, the set of versions to keep
# has not changed, so pruning could only drop the real previous version.
if [ "$previous" != "$rev7" ]; then
    for path in "$opt"/*; do
        [ -d "$path" ] || continue
        name=${path##*/}
        case "$name" in
            current|"$rev7"|"$previous") continue ;;
        esac
        if pgrep -f "$opt/$name/" >/dev/null 2>&1; then
            printf 'keeping %s: still in use by a running process\n' "$name"
        else
            rm -rf "$path"
            printf 'removed %s\n' "$name"
        fi
    done
fi

printf '%s is now current\n' "$rev7"

if [ ! -e "$wrapper" ] || grep -q "$marker" "$wrapper" 2>/dev/null; then
    mkdir -p "$bin_dir"
    wrapper_tmp="$bin_dir/.unreal.$$.tmp"
    cat > "$wrapper_tmp" <<'WRAPPER'
#!/bin/sh
# unreal-agent wrapper: exec the current version, resolved before the link moves
OPT="$HOME/.local/opt/unreal-agent"
exec "$OPT/$(readlink "$OPT/current")/unreal-agent" folders "$@"
WRAPPER
    chmod 0755 "$wrapper_tmp"
    mv -f "$wrapper_tmp" "$wrapper"
    wrapper_tmp=
fi

case ":$PATH:" in
    *":$HOME/.local/bin:"*) ;;
    *)
        printf '~/.local/bin is not on PATH; add the following line to your shell profile:\n'
        printf 'export PATH="$HOME/.local/bin:$PATH"\n'
        ;;
esac

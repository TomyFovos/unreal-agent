#!/usr/bin/env bash
# Candidate-only entrypoint. Existing installations, defaults and history stay
# owned by the ordinary launcher; no global link or configuration is installed.
set -euo pipefail
umask 077
kit_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
operation=${1:---offline}
if [ "$#" -gt 0 ]; then shift; fi
private_directory() {
  if [ -e "$1" ] || [ -L "$1" ]; then
    [ -d "$1" ] && [ ! -L "$1" ] && [ -O "$1" ] && [ "$(stat -c %a -- "$1")" = 700 ] || {
      printf '%s\n' 'HOLD_CANDIDATE_STATE_UNSAFE' >&2; exit 1;
    }
  else
    mkdir -m 700 -- "$1"
  fi
}
case "$operation" in
  --normal)
    [ "$(uname -s)" = Linux ] && [ "$(uname -m)" = x86_64 ] || exit 1
    # Candidate state cannot be redirected into an existing installation by a
    # later duplicate launcher flag. Fixture configuration uses the same owned
    # candidate/PWD path before launch, without a production bypass.
    for argument in "$@"; do
      case "$argument" in
        -config|--config|-config=*|--config=*|\
        -state-directory|--state-directory|-state-directory=*|--state-directory=*|\
        -session-directory|--session-directory|-session-directory=*|--session-directory=*|\
        -credential-directory|--credential-directory|-credential-directory=*|--credential-directory=*|\
        -socket|--socket|-socket=*|--socket=*)
          printf '%s\n' 'HOLD_CANDIDATE_STATE_OVERRIDE' >&2; exit 2 ;;
      esac
    done
    (cd -- "$kit_dir" && sha256sum --check --strict MANIFEST.sha256 > /dev/null)
	workspace=$(pwd -P)
	workspace_digest=$(printf '%s' "$workspace" | sha256sum)
	workspace_digest=${workspace_digest%% *}
	kit_digest=$(printf '%s' "$kit_dir" | sha256sum)
	kit_digest=${kit_digest%% *}
	private_directory "$kit_dir/normal-state"
	state="$kit_dir/normal-state/workspace-${workspace_digest:0:16}"
	private_directory "$state"
	# A stable, private and short socket pathname avoids Unix's pathname limit.
	socket_root="/tmp/unreal-initial-$(id -u)"
	private_directory "$socket_root"
	socket="$socket_root/${kit_digest:0:16}-${workspace_digest:0:16}.sock"
    # Preserve the user's workspace, HOME, existing CLI discovery and explicit
    # launcher settings. Native's own dedicated Owner enforces its HTTP policy.
    exec -a unreal "$kit_dir/unreal-agent" --config "$state/runtime.json" \
      --state-directory "$state" --session-directory "$state/sessions" \
      --credential-directory "$state/credentials" --socket "$socket" "$@"
    ;;
  --synthetic)
    [ "$#" -le 1 ] || exit 2
    scenario=${1:-success}
    case "$scenario" in
      success|bad-state|wrong-team|token-401|usage-429|scope-omitted|billing-unknown|model-missing|messages-500|response-unknown|body-unknown|login-metadata-only) ;;
      *) printf '%s\n' 'HOLD_INVALID_SYNTHETIC_CASE' >&2; exit 2 ;;
    esac
    (cd -- "$kit_dir" && sha256sum --check --strict MANIFEST.sha256 > /dev/null)
    private_directory "$kit_dir/synthetic-runs"
    runtime=$(mktemp -d "$kit_dir/synthetic-runs/run.XXXXXXXX")
    mkdir -m 700 -- "$runtime/home" "$runtime/tmp"
    # Built-in synthetic mode owns its loopback server and canary credentials;
    # it has no endpoint/Owner override and never opens the real Owner.
    source "$kit_dir/foreground.sh"
    r3_accounting_foreground_live 30s "$runtime" "$kit_dir/unreal-claude-auth-probe" \
      -mode=synthetic -fixture-case="$scenario" -state-dir="$runtime/state"
    ;;
  --offline|--status|--live)
    [ "$#" = 0 ] || { printf '%s\n' 'HOLD_INVALID_ARGUMENTS' >&2; exit 2; }
    # The original foreground runner retains the original Owner selector,
    # durable claim, manual generation bound, deadline and sanitized status.
    exec /bin/bash "$kit_dir/run.sh" "$operation"
    ;;
  *) printf '%s\n' 'HOLD_INVALID_ARGUMENTS' >&2; exit 2 ;;
esac

#!/usr/bin/env bash
# Shared foreground boundary. Only the source-bound runner chooses a command.
# The default helper suppresses stdout; the explicit Live helper preserves the
# trusted product's single sanitized JSON (saved by the product before print).
r3_accounting_foreground() {
  r3_accounting_foreground_mode suppressed "$@"
}
r3_accounting_foreground_live() {
  r3_accounting_foreground_mode visible "$@"
}
r3_accounting_foreground_mode() {
  local output=$1
  shift
  if [ "$#" -lt 3 ]; then return 2; fi
  local duration=$1 runtime=$2 binary=$3
  shift 3
  case "$duration" in
    ''|*[!0-9.s]*|*s*s*|s|.*) return 2 ;;
  esac
  [ -d "$runtime/home" ] && [ ! -L "$runtime/home" ] || return 2
  [ -d "$runtime/tmp" ] && [ ! -L "$runtime/tmp" ] || return 2
  [ "$output" = suppressed ] || [ "$output" = visible ] || return 2
  local child='' code=0 signaled=''
  local prior_int prior_term prior_hup
  prior_int=$(trap -p INT)
  prior_term=$(trap -p TERM)
  prior_hup=$(trap -p HUP)
  # Forward signals addressed only to the wrapper, as well as terminal-group
  # signals. GNU timeout forwards them to its child. No retries or second probe.
  trap 'signaled=INT; [ -z "$child" ] || kill -INT "$child" 2>/dev/null || :' INT
  trap 'signaled=TERM; [ -z "$child" ] || kill -TERM "$child" 2>/dev/null || :' TERM
  trap 'signaled=HUP; [ -z "$child" ] || kill -HUP "$child" 2>/dev/null || :' HUP
  # --foreground and explicit inherited stdin keep hidden terminal input in
  # the caller's foreground group even though we wait on a bounded child.
  # No tee, callback capture, environment inheritance or raw response log.
  if [ "$output" = visible ]; then
    env -i PATH=/usr/bin:/bin HOME="$runtime/home" TMPDIR="$runtime/tmp" \
      LANG=C.UTF-8 timeout --foreground --signal=TERM --kill-after=2s \
      "$duration" "$binary" "$@" <&0 &
  else
    env -i PATH=/usr/bin:/bin HOME="$runtime/home" TMPDIR="$runtime/tmp" \
      LANG=C.UTF-8 timeout --foreground --signal=TERM --kill-after=2s \
      "$duration" "$binary" "$@" <&0 > /dev/null &
  fi
  child=$!
  if [ -n "$signaled" ]; then kill -"$signaled" "$child" 2>/dev/null || :; fi
  while :; do
    if wait "$child"; then code=0; else code=$?; fi
    # A trapped signal interrupts wait. Reap the same child after forwarding;
    # do not return while the one-shot process is still active.
    kill -0 "$child" 2>/dev/null || break
  done
  trap - INT TERM HUP
  if [ -n "$prior_int" ]; then eval "$prior_int"; fi
  if [ -n "$prior_term" ]; then eval "$prior_term"; fi
  if [ -n "$prior_hup" ]; then eval "$prior_hup"; fi
  return "$code"
}

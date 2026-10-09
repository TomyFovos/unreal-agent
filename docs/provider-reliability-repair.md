# Provider reliability repair

Baseline: `9ce18a166a36ea4eb1a8b315472ec414af20a5f4` (Go 1.27, JSON v2).
These are synthetic reproductions and fixes of normal-path defects, not a claim
that a particular real Sandbox HTTP/SSE error has been diagnosed.

## Continuation projection

The disposable Builder derives ordered response groups and receipt dependencies
from canonical Turn/ModelResponse/ToolCallStatus records. It preserves original
private reasoning positions only for the same provider. Running and unreceived
receipts remain required across new input, repeated Build/send attempts and
resume. A later successful response at its canonical Turn input boundary marks
terminal receipts consumed; those closed groups return to bounded historical
selection/retrieval. History and Operation IDs are never rewritten.

Current native read bodies/revisions/range/truncation stay intact above the
individual large-result threshold. True mandatory overflow fails before HTTP.
When only private replay prevents fitting, a request-local whole-group portable
projection preserves public bodies before failing. Retrieved/reference/native
rendering is chosen for the group, preventing orphan calls or outputs. A final
typed validator also rejects contradictory native call definitions before send.

Secretguard remains unchanged. Receipt redactions contain only fixed vocabulary
and actual canonical states (including unavailable, running, failed and canceled).
Secret arguments make the whole group portable; original private replay IDs are
never retained with redacted arguments. Stubs are not canonical receipts.

Estimates remain conservative JSON/UTF-8 byte-based estimates, not measured
tokens. Explicit budgets, unknown-window fallback, schema and private framing
reserves remain. New initial selections use matching available CLI catalog
windows; canonical resume selections retain their identity.

## Synthetic verification

- Baseline A/C reproduce orphan native outputs; B reproduces an orphan call.
- Baseline 4–8KiB reads lose body/revision, and reasoning moves before commentary.
- Repair tests cover whole-group rendering, original order, redactions, repeated
  Build, incomplete async updates, bounded past history and true overflow.
- A loopback Docker OAuth CONNECT fake Host executes Bash/write/read/edit/read/
  Bash/Final. The fake model derives edit arguments only from the received native
  read result (128-byte, 4KiB, 8KiB and partial-range cases). HTTP/SSE failure and
  restart/resume preserve canonical receipts and actual files without repeating
  completed Operations. Fake credentials and workspaces are isolated.
- Existing permission/workspace/revision-conflict and both-provider tool tests
  remain required. No live Sessions, credentials, Sandbox or inference are used.

## Permission generations and Claude termination

The Host supplies a separate `BeforeGeneration` owner callback. Structured
RefreshContext invokes it on the Coordinator goroutine to replace only the
permission prompt layer and current constraints from the live Controller.
It does not rerun BeforeTurn or apply pending provider/model/effort/tool schemas.
Operation admission remains authoritative; a change after prompt construction
can still deny or cancel an Operation. Action/replay identity excludes permission
revision; old successes, denials and cancellations retain their canonical outcome.

A stopped Controller reports `Stopped=true`, effective Read Only and the
historical `SelectedMode`. It grants nothing. Gateway/TUI show expired/Session
stopped; resume creates a fresh Read Only Controller. A live detach preserves
the grant. Historical Operation permission evidence is not changed.

Each independent Claude generation has a generation deadline through validated
candidate result. Post-candidate termination has a separate **2 second** finite
grace: close stdin, allow cleanup, request SIGTERM at halfway if still alive,
validate every remaining stdout frame through physical EOF, then reap the parent
and kill its process group. Only validated heartbeat/correlated serializer
metadata can follow a result. Invalid trailing tools, drain timeout or inability
to reap accept no Action/Final. On failure only, a pipe may be closed to unblock
cleanup; this is never manufactured success EOF. Owner cancellation applies at
every stage. Fixed stages distinguish generation, termination and reap timeout.
Cleanup time is excluded from client-clock generation speed statistics.

Fixtures inject short finite grace/timeout values, test slow valid cleanup,
metadata, serializer retries, invalid/Bash tails, held pipes, no response and
cancel. No tool callback runs before EOF/reap. The existing prepare/version/help/
auth checks remain outside the Action round loop, not cached for Host lifetime.
The fake Host changes Read Only → Full Access → Read Only during inner rounds,
replays a denied ID after promotion, executes a new ID, and leaves a pending
model/effort switch unapplied. The common-package checkpoint follows separately.

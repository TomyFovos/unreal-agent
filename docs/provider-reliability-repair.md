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

Terminal receipts retain `WaitingFor` as dependency evidence, not liveness.
Redaction uses the Coordinator's running flag and actual Operation states. The
aggregate precedence is running, rejected, unknown, failed, canceled, completed;
an absent canonical receipt is always unavailable. `operation_states` is a sorted
multiset of the actual per-Operation states (including duplicate counts), with no
IDs. An Operation denial makes the aggregate rejected without rewriting that
Operation's terminal status. Neither projection nor replay erases dependencies.

Estimates remain conservative JSON/UTF-8 byte-based estimates, not measured
tokens. Explicit budgets, unknown-window fallback, schema and private framing
reserves remain. New initial selections use matching available CLI catalog
windows; canonical resume selections retain their identity.
The real Host creation path persists a same-model selection/applied pair before
the first Turn when a known window exists. This keeps catalog metadata outside
immutable configuration while binding it durably. Unknown windows keep revision
zero and fallback; resume never silently refreshes the recorded window.

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
model/effort switch unapplied.

## Common distribution

`scripts/common_development_bundle.py` packages the inspected snapshot using
the existing CGO kit builder. One archive contains the four binaries, installer,
doctor, native-discovery wrapper, assets, README and exact checksum manifest.
It has no Sandbox/provider identity and contains no credentials/config/history.
Legacy daily/Pilot scripts remain compatibility assets, not the new startup path.

The wrapper reuses existing Git-root validation and private atomic/owned-file
helpers, then calls native `unreal` without `--config`, with a repo-specific
XDG config root plus state/socket. Native bootstrap is the only runtime writer.
DiscoverProviders stays enabled; authentication/catalogs and initial provider
choice remain owned by the existing normal launcher. Docker OAuth is explicitly
opted in with the validated forward proxy; Claude Team trust requires audit AND
operator opt-in. Neither is inferred from a Sandbox name or CLI presence.
Model/effort are chosen through the existing initial chooser and `/model`.

Read Only/human-reviewed ceiling are the normal bootstrap defaults, not a grant.
Compared with the old short Pilot, this uses ordinary defaults: Claude
MaxActionRounds **32** (old 8), TimeoutMillis **300000** (old 120000), ResultBytes
**65536** (old 4096), Codex MaxAttempts **2** (old 1). These limits are unchanged
production defaults, not evidence for the continuation repair or permission
approval. Explicit configs and budgets remain authoritative.

Fresh Bash discovery, exact owned legacy replacement with opt-in/private backup,
reinstall, repo/worktree separation and alias/function conflicts are tested.
The installer never deletes parent-shell functions, old kits, config or history.
`unreal --instance repair-v2 NAME` explicitly separates an incompatible old Host.
Gateway identity includes executable SHA256 + protocol revision + existing path
bindings; unknown/mismatched identity fails closed without stopping any Host.

`unreal --readme` displays detailed instructions; startup displays only four
controls and ambient-access warning. Interactive Enter waits without expiry;
noninteractive startup does not wait. `unreal --acceptance-fixture repair`
explicitly creates a new synthetic Git workspace for later human acceptance,
without discovering or editing real projects or automatically sending inference.

## Acceptance matrix and evidence

| Case | Offline path and assertion |
| --- | --- |
| T1 A/B/C | Real Builder/Engine → Responses encoder; whole-group reference/retrieval/OMIT, no orphan native frames; safe normal cases continue. |
| T2 read → edit | Native fake Host reads 128-byte, 4KiB, 8KiB and partial results; the backend obtains exact edit revision/content only from the received receipt. |
| T3 secret results | Unchanged secretguard; actual completed/running/failed/canceled/unavailable state stubs; sensitive arguments become wholly portable. |
| T4 order/async | Ordered reasoning/commentary/calls, running→final, new input, failed send, cancel/resume and provider switch; repeated Build does not acknowledge receipts. |
| T5 budget | Initial canonical catalog window equals picker metadata, unknown fallback and explicit budget; 500/1000-turn history and final-wire fuzz; genuine overflow sends no HTTP. |
| T6 live permissions | Fake Claude Host changes Read Only→Full→Read Only between generations; old denied ID is replayed, new ID is rejudged; pending model change stays pinned. |
| T7 lifetime/UI | Stopped snapshot/picker is expired, live detach keeps its mode, resume is Read Only; existing Ask/cancel/deny/child tests retained. |
| T8 termination | EOF/metadata, delayed cleanup, serializer retry, illegal tools/tails, held child pipes, deadline/cancel; no unverified proposal executes. |
| T9 fake Host | Loopback Docker CONNECT proxy, native Bash/read/edit/read/Bash/Final; deliberately injected HTTP/SSE failure, receipt/file verification and duplicate-free resume. |
| T10 common product | Normal discovery matrix and strict proxy/trust fixtures; one Host routes both picker choices into real fake-provider requests/receipts; installer/PTY, identity, worktree isolation and final packaged-binary checks. |

The original-source reproductions fail before the fixes and pass afterwards;
the evidence bundle retains both exits, including intermediate fixture failures.
The execution/log codec fuzzer's former fixed 256KiB fixture was insufficient for
its 70KiB error/resume seed now that unreceived groups keep their full bodies.
Only that fake model's explicit window is sized from its bounded test payload.
Production budgets, true-overflow assertions and normal read thresholds are not
increased. The catalog refresh oracle counts one new metadata request per
refresh, allowing the newly required initial catalog lookup without inference.

Legacy Pilot control clients now also provide the native executable SHA256 and
protocol identity. Matching paths alone never satisfy the new Host check;
old archives remain untouched and unverifiable old Hosts fail closed.

Run `make test-claude-structured`, the Python installer regressions,
`go test -race -p 1 ./...`, `make check`, and isolated
`make build BIN_DIR=<new-directory>`. After committing the verified sources,
`scripts/snapshot.py build` validates GoReleaser and the final CGO archive.
`UNREAL_COMMON_KIT_DIRECTORY=<extracted-kit>` enables the actual installer,
doctor, fresh-Bash, reinstall and invalid-environment Python checks;
`UNREAL_COMMON_BIN_DIR=<extracted-kit>` enables the two-provider fake Host
routing/receipt test against the exact distribution executable.

Optional legacy packaged tests skip unless their explicitly verified historical
kit variables are set. These skips are reported separately, not counted as
passes. Required common-product packaged tests run with the new archive.
Command/exit-code logs and commit/file correspondence are included in the review
ZIP. Real Session reads, authentication, Sandbox acceptance and real inference
remain outside this repair's evidence; formal Release reproducibility is not
claimed for a worktree with protected untracked repositories/files.

Source verification on Go **1.27.1** / JSON v2: continuation final-wire/fake
Host, permission-generation/termination and common-product focused tests PASS;
`make test-claude-structured`, `go test -race -p 1 ./...`, `make check` and
isolated `make build` PASS. The Python regression runs **117** cases: **99**
executed successfully, **18** explicitly skipped pending packaged inputs.
Three of those are the common final-archive checks run after this source commit;
the other fifteen require historical optional kits and remain unverified.
The final-archive installer/doctor, two-provider executable and checksum results
are collected post-build in the review bundle, separately from source gates.

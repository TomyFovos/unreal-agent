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

The later permission/lifecycle and common-package sections are completed with
their independently verified checkpoints; the continuation checkpoint adds no
authentication, permission or provider fallback.

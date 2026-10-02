# Long-lived Host

The `harness/host` API owns a Coordinator per session. Create, Resume and Open acquire a nonblocking process-shared writer lock before touching the log. Create refuses an existing ID. A stable lock sidecar is retained; process death releases ownership. The state directory must be private to the harness user. The low-level Store remains an unsynchronized implementation detail: all writers must participate in Host ownership.

Supply a fresh runtime factory for each session, an immutable non-secret configuration identity, and a lifetime context. The factory must bind its operation manager and handlers to the supplied session context. Disconnecting or canceling a Subscribe or Submit call does not cancel that context. Explicit Stop or Host.Close ends execution. Host.Close drains managers before releasing ownership. Interactive lifecycle instructions tell the model to wait for another input; the legacy runner supplies one-shot lifecycle and StopWhenIdle.

## Delivery and recovery

Submit requires the current generation and a stable Inbox ID. Its receipt is emitted only after AppendInput commits. Concurrent identical submissions share the receipt; payload conflicts and stale generations fail explicitly. Lost receipts can be recovered by resubmitting after Resume. Controls use the same contract. The receipt means durable delivery, not effect completion. Stop completion is a separate canonical Host record; pending stops are recovered before dispatching operations. A restored hard stop cancels work without starting it.

Stop is a convenience that generates a fresh ID. For retryable control delivery, callers should construct a control Input with a stable ID and call Submit. ACKs are not Inbox inputs and never wake the model.

Inspect returns a history page plus current operation snapshots. Subscribe atomically installs a bounded subscriber and takes its initial snapshot. History cursors are Item.Sequence. Revisions and generations are transient; reconnect must inspect/resubscribe. SaveOperation emits a post-commit operation notification. A slow subscriber receives gap then closure and must resync; intermediate operation transitions need not be replayed. Consumers must never infer receipt or effect completion from transient notifications alone.

## Compatibility

New logs use file format 3, adding versioned host_record items (configuration and stop_complete). Version 2 remains readable. Host explicitly upgrades v2 under writer ownership before adding metadata: the exact original is fsynced to an exclusive .v2 backup, then a v3 header is atomically published with unchanged history. Conflicting backups and unknown versions fail without replacement. Existing fork/recovery semantics and cursors are preserved; metadata is excluded from model context. Old readers reject v3 instead of silently dropping records.

The one-shot runner consumes this same Host and outputs canonical items, including metadata. Its optional JSONL output is a projection of the canonical session: failed output may contain only a prefix, and never blocks persistence through a Store observer. Workspace .env values are read into a session-local overlay; no process-wide environment is changed.

## Verification

Race tests cover concurrent delivery, lost ACK after restart, conflict, stale generations, persistence failure, stop-effect recovery, snapshot/subscription barriers, operation notifications, slow subscribers, and real process lock death/reacquisition. Store tests cover v2 migration and unknown versions. Existing runner, fork/recovery, and execution-log fuzz regressions remain enabled.

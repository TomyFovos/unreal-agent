# Child Session and Operation Viewer

This package is an observation and control client, not a scheduler. Its projection
comes from Host.View snapshots, ordered post-commit Host.Event values and persisted
history pages. It never opens a Store, acquires a writer lock, starts a process,
or invokes an LLM.

## Embed in a frontend

Create a Model with SubagentOptions(), then NewClient(reader, model, controls).
ParentReader scopes reads to an attached parent and the direct children identified
by its canonical SubagentStart operations. HostControls delegates to the parent
Host through subagent.SteerChild, CancelChild and ResumeChild. The reader matches the local Host gateway's
Inspect and Subscribe methods. DecodeChild must accept only a versioned Subagent
start plan. DecodeFinish must accept only the child's canonical explicit Finish
record, never a parent handle's cached result or assistant text. The Finish OperationID belongs to the child Finish operation;
it is not the parent SubagentStart operation ID.

Watch(ctx, sessionID, onChange) owns one subscription per session. Refresh is a
read-only one-shot rebuild (also useful for archived/offline children). A second
Watch or Refresh cannot replace an active watch's epoch. The frontend selects a
row using Model.Select, obtains Model.Rows and Model.Detail, and can embed
RenderRows / RenderDetail. For arbitrary persisted transcript pages, call
Client.History with the returned NextAfter cursor; paging does not mutate token
aggregates. Detail.Recent is a bounded cache, not the full transcript.

Cancel the Watch context and wait for Watch to return before leaving a panel or
replacing its Reader. Callbacks run in the observation goroutine and should only
enqueue a UI redraw. A frontend can watch the parent and the currently selected
child; it does not need to load every child's transcript at once.

## Truth and recovery

- The latest parent Operation owns parent-facing execution/cancellation state.
- Only child canonical Finish owns the child's result, files, tests and blockers.
  A stopped process, canceled parent operation or final-looking assistant message
  does not imply Finish.
- Runtime liveness is transient and separately labelled. Missing generation,
  disconnection, or a gap yields unknown. No PID or heartbeat is a durable result.
- Historical operation snapshots cannot overwrite a newer latest-operation view.
  Duplicated revisions are ignored. Missing revision/sequence triggers a fresh
  subscription and canonical rebuild. Obsolete-generation events cannot overwrite
  the current epoch.
- Snapshot pagination completes before live events are consumed. Events older
  than the final snapshot revision are redundant; later missing events resync.
- Token usage counts persisted model responses, deduplicated by session/turn and
  response ID (sequence when the provider supplied no ID). Replay/reconnect never
  adds it twice. Input includes cached input; output includes reasoning tokens.
  Missing accounting is unknown; incomplete history/accounting is marked partial.
  Zero totals are known only when the provider supplied raw accounting.
- Session elapsed is wall time from canonical creation to Finish, or to now while
  runtime liveness is known running. It includes downtime; it is not CPU/active
  runtime. Operation elapsed requires canonical ready/terminal timestamps.
  A terminal operation snapshot without a terminal timestamp remains unknown.
- The transcript cache is bounded (128 items by default, at most 4096).
  Aggregate deduplication retains response identities, not full response bodies.

## Controls

Controls is optional. Each request includes parent identity, observed parent
generation, parent Operation ID, derived child identity, and a caller-generated
stable InputID. The adapter must validate the relationship at the owning Host,
persist the intent through its existing control/Inbox path, and enforce ownership
and permission. Do not call child send handlers, Store.Create/Resume, process
launchers or a second scheduler from a UI.

Steer, Cancel and Resume forward requests through this adapter. They do not
optimistically alter canonical displayed state. Reuse InputID after transport
failure. Concurrent controls for one child are rejected locally; successful
repeated request IDs return their prior receipt. The owning Host remains the
authority for idempotency and duplicate resume across clients/processes.

All render helpers remove terminal control characters. A UI rendering raw detail
or transcript values must also escape/sanitize external text.

The separate child process has no live Host subscription in the parent process.
ParentReader exposes read-only child snapshots with an empty generation and
unknown runtime liveness. A frontend may refresh the selected child periodically;
this reads persisted history and never asks the parent LLM to poll a child.
ResumeChild verifies that a live parent already owns a nonterminal child. It does
not launch a second process. Resume a stopped parent explicitly through the Host
ownership gate; its existing operations recover the same children.

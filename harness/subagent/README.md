# Asynchronous Child Agents (`harness/subagent`)

**Language:** English | [日本語](../../docs/ja/harness/subagent/README.md)

`harness/subagent` manages asynchronous child agent subprocesses (`unreal-agent child --stdio`), bidirectional parent-child messaging, and durable lifecycle recovery.

---

## 1. Parent & Child Composition

### Parent Host Setup

1. Retrieve the parent Session owner via `host.SessionFromContext(ctx)`.
2. Construct `subagent.NewManager(ctx, Config{Owner: owner, Directory: sessionDirectory, Binary: absoluteAgentBinary, Arguments: []string{"child", "--stdio"}, Templates: allowedTemplates})`.
3. Register the manager with `LocalOperationManager`, add `tool/subagent.Extensions(owner.ID, templates, false)` to the tool registry, and call `Close` when the parent runtime shuts down.
4. Bind `Runtime.InterruptedInput = manager.InterruptedInput` and `Runtime.CompletedInput = manager.CompletedInput`. The Coordinator records completion notices before deciding whether a one-shot Session can stop, after the active model call has had a chance to read the child's report itself.

- **Template security:** Binary paths, arguments, provider/profile selections, and capability bounds are fixed in trusted Host templates (`Subagents` config). The model can only pick a registered template name and supply a task string.
- **Permission intersection:** Child tool capabilities are always a subset of the parent's policy. No credentials are serialized into child plans.

### Child Process (`unreal-agent child --stdio`)

Calls `subagent.Serve(ctx, os.Stdin, os.Stdout, childFactory)`:
- Opens a child Host (`Lifecycle="child"`, `Heartbeat=0`) with tools restricted to bounded executors plus `SendParent` and `Finish`.
- Discovers its workspace's current `AGENTS.md` at each new child start; resume/restart replay the child's saved snapshot. The shared `MutationStateDirectory` keeps multi-process file locking coordinated.

---

## 2. Wire Protocol, Persistence, and Completion

- **Deterministic Child ID:** Derived from the parent Session ID and public `SubagentStart` Operation ID.
- **Length-prefixed JSON wire protocol:** 4-byte big-endian length header followed by versioned JSON (length encoded as uint32) over stdio pipes.
- **Commit-before-ACK peer messaging:** Parent-child messages are persisted as canonical `InputPeer` records before an ACK is returned, so acknowledged inputs survive reconnects.
- **Explicit `Finish` contract:** Successful tool-capable children report with `Finish`, which is rejected while other calls or Operations remain pending. A child that ends a turn without Finish and has no continuation receives a failed canonical `HostRecord(kind=finish)`, identified by its model turn and carrying the stop reason. This includes empty, reasoning-only, refused, output-limited, and text-only responses from a tool-capable child. Pending tools/approvals, children, and an unanswered non-progress `SendParent` preserve their existing waits. Output-limit responses are not retried automatically; their parent decides what work to request next. Text-only child runtimes retain their existing completion contract.
- **Graceful cancellation:** Persists a cancel control input and `canceling` checkpoint, sends a stop request to the child, and escalates to process-group termination if the child does not exit within 2 seconds.
- **Host restart:** Checks the child's process-shared writer lease before reconciling it. A live child retains its Operation while reconnecting; the parent observes its canonical completion without spawning another process. A gone child without Finish receives one `HostRecord(kind=ended)` with `ended_by=host_restart` and `ended_at` equal to its last recorded timestamp. The parent receives a stable ended notice with a restart/report action; repeated restarts do not duplicate it. This is executor-loss evidence, not a fabricated Finish result.
- **Channel & message integrity:** Each pipe channel fixes its connection, sender, and target identities (stale or mismatched frames are rejected). Retrying with the same input ID and payload returns the original receipt, while a changed payload returns a conflict. `ready` and progress notices travel separately from final completion, which is proven only by a canonical `Finish` record.

---

## 3. Operator Control APIs

- `ReadChild` — Reads a bounded canonical history page, active Operations, `ChildConfig`, and `Finish` status.
- `SteerChild` — Sends a follow-up message to a running child via a durable parent operation-intent input (with idempotent `InputID`).
- `CancelChild` — Cancels a running child Operation.
- `ResumeChild` — Verifies that a live parent Host owns and has recovered a non-terminal child (never spawns duplicate child processes).

> **Control Validation:** All control requests validate the current parent ID/generation and the exact `SubagentStart` Operation/child pair using a stable non-empty `InputID`. Terminal (`completed`, `failed`, or `canceled`) children cannot be resumed.

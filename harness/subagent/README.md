# Asynchronous Child Agents (`harness/subagent`)

**Language:** English | [日本語](../../docs/ja/harness/subagent/README.md)

`harness/subagent` manages asynchronous child agent subprocesses (`unreal-agent child --stdio`), bidirectional parent-child messaging, and durable lifecycle recovery.

---

## 1. Parent & Child Composition

### Parent Host Setup

1. Retrieve the parent Session owner via `host.SessionFromContext(ctx)`.
2. Construct `subagent.NewManager(ctx, Config{Owner: owner, Directory: sessionDirectory, Binary: absoluteAgentBinary, Arguments: []string{"child", "--stdio"}, Templates: allowedTemplates})`.
3. Register the manager with `LocalOperationManager`, add `tool/subagent.Extensions(owner.ID, templates, false)` to the tool registry, and call `Close` when the parent runtime shuts down.
4. Bind `Runtime.InterruptedInput = manager.InterruptedInput` and `Runtime.CompletedInput = manager.CompletedInput`. The Coordinator records completion notices at a model read boundary after reconciling tool receipts that may already contain the report.

- **Template security:** Binary paths, process arguments, and capability bounds are fixed in trusted Host templates (`Subagents` config). The model supplies the task and optional brief fields. Runtime selections use the Host's registered resolver.
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
- **Parent read boundary.** The next model request carries queued peer messages in inbox order. Claude Code receives them through `PostToolBatch` and Responses providers receive them with tool results. The corresponding model response confirms delivery for replay. `ready` stays out of model context and progress can ride along with a request. Both leave an idle parent asleep.
- **Launch brief and routing.** The start checkpoint and child configuration retain the same launch brief. `SendParent` with exactly `brief <field>` can return a configured field verbatim in the child Operation receipt, which records the rule ID and field locator. Missing values and other questions use the normal parent inbox. A completion notice carries the canonical Finish report in full. The existing terminal tool receipt path also carries the report. Acceptance belongs to the parent. [Delegation brief](../../docs/usage.md#delegation-brief)
- **Parent turn failure.** A model error ends the parent's turn and leaves the child manager running. Peer messages and reports still commit to the parent inbox. Pending child approvals remain available to the next parent turn. User input, a non-quiet child message, or a scheduled usage reset can wake the parent under the existing rules. An input from the failed request stays in context and needs a new wake event.
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
- `ReviewChild` — Optionally appends the parent's `accept` or `reject` assessment. It does not change the child's finish, permissions or scheduling. From the parent's shell: `unreal-agent subagent review HANDLE accept|reject [--reason TEXT]`. Reason is optional; retries with the same input identity are idempotent. A later assessment is another record, preserving earlier rejection events.

Tool-capable leads use `SubagentReview` with `handle`, `decision` (`accept` or `reject`) and optional `reason`. This reaches the same parent writer without needing Claude's CLI socket environment.

> **Control Validation:** All control requests validate the current parent ID/generation and the exact `SubagentStart` Operation/child pair using a stable non-empty `InputID`. Terminal (`completed`, `failed`, or `canceled`) children cannot be resumed.

## 4. Layer and worker-model analysis

`/analyze` → **Layers** reads the Session tree on demand, including archived children and grandchildren. The same statistics appear in its JSON and Markdown exports. New `HostRecord(version=1, kind=activity)` child-start records include the layer and resolved provider/model/effort. The role mapping used by model selection also derives layers for older starts; custom templates are `unknown`. Roles do not imply any particular provider.

Built-in worker and collector templates started directly by the main Session use the lead role's provider, model and effort and record the `lead` layer. Worker and collector role selections apply below a lead, including deeper descendants below workers. Starts still succeed under the existing selection and override rules. The main delegates hands-on work through a lead, which reviews its children's results. Analysis uses the resolved layer from the start plan, child configuration or startup activity; records without a layer retain their historical template-based attribution.

Generation TPS uses only reported output paired with response timing. `provider_api` timing is measured; `client_clock` timing is derived. Missing timing stays `unknown`, and requests without timing do not inflate the numerator of a timed TPS average. Separately recorded compaction usage contributes to volume. Request counts are canonical responses and recorded compactions; unreported provider-internal calls cannot be counted. Wait time uses `usage_limit_wait` start/clear pairs, never the planned retry deadline as measured duration. First response means the first completed response, not the first streamed token.

Rework's denominator is normally ended workers with an explicit assessment. The latest accept/reject supplies the assessment counts; any explicit rejection or subsequent same-parent, same-title delegation contributes to the reviewed worker's rework share. Repeat-title inference is displayed separately and never creates an explicit review. Unreviewed normal ends, failed ends and cancellations are separate. Reuse the original one-line task title for rework. Record review decisions when useful and describe verification in the parent's report; neither is a completion requirement.

Task chains are derived from those repeat-title relationships. Totals include all attempts and are attributed to the initial worker model; mixed-model tasks are flagged. Both summed worker wall time and task elapsed time including parent gaps are shown. Parent tokens cannot be reliably attributed to an individual task and remain in the layer totals. Reopening `/analyze` refreshes the descendant snapshot, whose observation time is displayed. Missing descendants mark totals partial.

List-price USD is a derived equivalent, not actual subscription billing. The offline table currently knows the exact OpenCode Go `deepseek-v4.1-flash` and `qwen3.8-flash` identities from the checked-in 2026-11-05 public model-list snapshot (including DeepSeek's advertised time multiplier); other models and missing usage fields stay unknown. Analysis exports contain metadata/counters and opaque task IDs, excluding task bodies and review reasons.

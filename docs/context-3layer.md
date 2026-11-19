# Three-layer context

The provider-neutral Host API provides raw evidence (L3) and the current state (L2). The three-layer design is tracked in [Issue #41](https://github.com/TomyFovos/unreal-agent/issues/41).

## Read canonical evidence

`record show LOCATOR` reads a locator. `record find TEXT` searches committed records in that Session and its descendants, including archived children. Their native tools are `RecordShow` and `RecordFind`.

- `rec:<session>/<sequence>` returns an exact JSONL item, including its line ending. Suffixes select the original user, peer, or assistant text, a recorded tool call, a child Finish, or a captured artifact. Child Finish locators include the child handle and can select `summary`, `tests`, `changed-files`, `blockers`, or a recorded `next` field.
- `rec:<session>/output/<operation>/<stream>` selects a terminal operation stream. Shell stdout and stderr come from their existing capture files. Remote jobs retain original bytes when their existing display value differs. Older previews whose originals are unavailable yield an explicit error. Standalone operation checkpoints also have locators for their exact JSONL bytes.
- `rec:<session>/<sequence>/tool-error` reads the original error text of a tool call that failed before starting an operation.
- `Session.RecordArtifact` explicitly captures a file version. The locator includes its path and SHA-256. Session-owned attachments preserve captured versions when the workspace copy changes or is removed.
- `?bytes=<start>-<end>` selects a zero-based half-open byte range. `?line=<number>` selects a one-based line with its original ending. The caller can request pages of a chosen byte size and follow the explicit continuation locator. Zero requests the entire selection.
- Find filters by kind, handle, inclusive start time, exclusive end time, and case-sensitive substring. Excerpts use the exact match, a first line, or a literal token from a structured record. Time filters require a recorded timestamp.

`record show` writes the selected bytes directly. `--json` and `--page-bytes` return text or base64 and the continuation locator when present. `record find` returns JSON matches and supports `--kind`, `--handle`, `--from` and `--before`. Times use RFC3339.

The processed counts in L2 link to `rec:<session>/context/<slot>`. The children index lists the Session's child handles, statuses, and canonical operation and report locators. Other indexes list source locators for the slot's evidence.

The Host uses the Session's current read permissions for file-backed sources. Session history resolves through the same committed-log and archive readers used for inspection. Input receipts and observations carry source locators. A mid-turn continuation preserves the locators it offered, and a subsequent response still acknowledges delivery through the existing input watermark.

## Rebuild current state

L2 has thirteen slots. They are `session`, `requests`, `rules`, `plan`, `decisions`, `children`, `approvals`, `waiting`, `questions_to_user`, `artifacts`, `issues`, `recent_events`, and `metrics`. Entries carry source locators. Active human requests carry the full original text, and mixed input retains its alias and provenance. Processed items keep a count and an L3 index locator.

S6 lists unfinished children and ended children whose review is pending. An explicit accept, reject, or cancellation settles a child. For an older child with no review, the projection settles it after the parent's request includes its terminal report and produces a recorded response. Delivery can come from a terminal peer notice, a child tool receipt, or a recorded show or wait result. A terminal operation checkpoint alone leaves the review pending. Children that ended during the current active human request keep their pending review even after the parent has seen the report. Settled children are counted by template, model, and ending. Explicit review statistics use the recorded reviews.

S10 lists every path reported by a child in S6 and every path the coordinator wrote since the last semantic boundary. Before the first boundary, coordinator writes are eligible from the start of the Session. A path shared by multiple writers appears once with its latest writer. Other paths contribute to the processed count.

`Session.AppendContextEvent` commits an explicit `context_state_event` to the same JSONL as the conversation. `Session.ContextState` returns a detached snapshot. The reducer applies each event ID once, and retrying an append returns the first event's locator. The restore checkpoint stores the reduced state and applied IDs. Full replay and checkpoint plus tail produce the same state bytes. A fork retains inherited events as evidence and starts with its own current state.

The writer and replay derive machine events from the same canonical inputs, responses, permission records, and operation checkpoints. A child start carries the recorded brief. Progress goes into recent events, together with observed routing rule IDs. Child completions carry the Finish locator when a report exists. Older checkpoints that embed a Finish can be read at the checkpoint locator. S7, S8, and S11 carry open approvals, waits, and issues. Resolutions, replacements, and child endings settle the corresponding entries. S9 carries visible `【要確認】` questions until the next human input or an explicit answer link. The next human input settles the L2 wait. The coordinator can still record which message answered the question.

Artifact paths come from changed files in Finish reports and successful write receipts. An explicit capture links a stored version of a file. File contents are available from that version or through the normal file read tools.

The log preserves unknown event versions and kinds for a future reader. The reducer applies the versions and kinds it understands. `state show` and `StateShow` read the latest L2 on demand.

## Rebuild the Responses request window

The first rebuild can occur as soon as the complete input reaches W, including the first request after upgrading a long-running Session. Each rebuild fixes the system instructions, tool schemas, and a copy of L2 with evidence locators. The raw tail starts at the last human message when that fits. When it exceeds the rebuild target, the Host advances the cut to the newest boundary that preserves undelivered inputs, the in-flight turn, and whole tool call and receipt groups. The L2 copy keeps the active human request in `requests.current`, including its original whitespace and line endings. The L1 head gives the locator range for earlier conversation in L3, where `record show` and `record find` can retrieve it.

The rebuilt input targets fewer than W tokens. If the required minimum exceeds W and fits the input budget, the boundary records its size and keeps the full required projection. If the system instructions, tool schemas, L2, and required tail together exceed the input budget, the request uses the configured provider's existing `summary` path. The `window_fallback` record gives the reason `l2_over_budget`, the input budget, and the sizes of the required parts. The next request can rebuild the three-layer window. Responses history carries public messages and matched tool receipts. Thinking and new model summaries stay out of the three-layer log.

After a rebuild, an estimated W tokens of growth triggers the next size-based rebuild. An input budget overflow, provider overflow, or header change can trigger one sooner. Within a window, new entries append in recorded order. Large text tool results display a head and tail excerpt with locators for the original streams or receipt. Native image results retain their typed bodies. For a large source, `RecordShow` can return a byte range or page that fits the configured excerpt size.

The `window_boundary` record stores the causes, snapshot sequence, retained range, before and after input token counts, and whether the required minimum exceeded the target. The counts identify the estimator and whether the prior input used measured prefix usage. The first following Turn links to that boundary, and its response records cache read and write counts when reported. Reopening from a restore checkpoint reuses the recorded L2 copy and tail rules.

These keys are under `Context` in `runtime.json`.

| Key | Default | Purpose |
| --- | --- | --- |
| `WindowStrategy` | `three-layer` on Responses and the Claude Code subscription route | Set `summary` to use the summary strategy. |
| `WindowThresholdTokens` | `200000` | W for the first rebuild, the size target for rebuilt input, and the growth threshold after a rebuild. |
| `L2TargetTokens` | `5000` | The target recorded alongside the actual copy size. All current state remains eligible above this target. |
| `RecentEventCount` | `0` | Use events since the previous boundary. At the first boundary, use events since the start of the active human request. A positive value selects the latest N for the copy. The full event history remains available. |
| `ToolOutputExcerptChars` | `0` | Use the existing tool display allowance. A positive value sets the combined head and tail size in Unicode characters for each text part. |
| `CacheRewarmMinutes` | `0` | Optional interval recorded for the PR7 cache comparison. Runtime requests remain driven by normal Session activity. |

## Rebuild the Claude Code window

The subscription route uses the same boundary conditions and tail selection. At a boundary, Unreal starts `claude -p` with a fresh `--session-id`. The CLI receives Unreal's usual appended system prompt and the new window's L2 copy and public L1 tail. Signed thinking remains in the old Claude transcript on disk.

The `provider_conversation_binding` record links the Unreal Session and window boundary to Claude Code's conversation ID. Between boundaries and after Host restart, `--resume` uses that ID and sends the new input. Unreal owns children, approval requests, unanswered questions and usage-limit waits.

At each main `PostToolBatch`, Unreal waits for approvals and records terminal tool receipts. It checks the latest model call's input tokens, including cache reads and writes. When a rebuild is due, Unreal includes queued inputs in the new window and records a boundary with the `mid_turn` cause. It returns `continue: false` with a `stopReason`, then starts a new conversation after the CLI ends the turn. Within a window, `PostToolBatch` sends queued inputs to the next call in the same conversation. A rebuild due after a final answer happens at the next request. [Claude Code PostToolBatch hook](https://code.claude.com/docs/en/hooks#posttoolbatch)

Admission estimates the rendered window text and the request's system and tool header. Subsequent turns can use the last reported model call's full input size with the estimated change in text. The first request's cache record uses the first main model call's counters, even if the CLI makes more model calls in the same turn.

For the three-layer strategy, Unreal sets `DISABLE_AUTO_COMPACT=1` for each launch, including in its per-launch `--settings`. Claude Code 2.1.x uses this variable to disable context-limit and idle auto-compaction in the CLI. Unreal's idle `/compact` is also disabled in this strategy. The `summary` strategy uses the CLI's `--autocompact` window and Unreal's idle maintenance. [Environment variables](https://code.claude.com/docs/en/env-vars) · [CLI reference](https://code.claude.com/docs/en/cli-reference)

The Claude Code `opus` alias resolves to Opus 5.5, which defaults to a 1M-token model context. Unreal defaults to rebuilding at 200K. Claude Code's `/context` displays usage and distinguishes the CLI's compaction window from the model limit. With automatic compaction disabled, exceeding the model limit ends the headless turn with `Prompt is too long`. [Claude Code model configuration](https://code.claude.com/docs/en/model-config#extended-context) · [Claude Code error reference](https://code.claude.com/docs/en/errors#prompt-is-too-long)

## Record coordinator decisions

Responses providers use the native tool names. A Claude Code main uses the Host's executable from its Session shell, with the same socket as `unreal-agent subagent`. These commands work in Read Only mode and evidence reads use the Session's normal permissions.

| CLI after `unreal-agent` | Native tool | What it records |
| --- | --- | --- |
| `state show` | `StateShow` | Return the current state, including open question IDs and evidence locators. |
| `state decide TEXT [--why TEXT]` | `StateDecide` | Record a decision and optional evidence, alternatives, and related IDs. |
| `state plan ID --text TEXT [--status STATUS]` | `StatePlan` | Add or update a step. Position, owner, dependencies, and decision IDs can be set explicitly. |
| `state rule add EXCERPT --source LOCATOR` | `StateRuleAdd` | Keep an exact quote from the specified user message. |
| `state accept HANDLE` or `state reject HANDLE [--why TEXT]` | `StateAccept` or `StateReject` | Settle the owned child's review. The reason is optional. |
| `state question TEXT [--id ID] [--source LOCATOR]` | `StateQuestion` | Record a note about a question. Send the question in the visible reply. The source can link an existing assistant record. |
| `state answer ID --source LOCATOR` | `StateAnswer` | Link the open question to the user message that answers it and settle the wait. |

`state question answer` is also accepted. The CLI can take multiline text from stdin with `-`. Use `--help` after a command for its flags.

Both state review commands and `subagent review HANDLE accept|reject [--reason TEXT]` write the same canonical review. L2 derives its child update from that review, and `/analyze` uses it for the reviewed-child rejection rate. The child's recorded execution result is independent of the review.

## Measuring

Open `/analyze` → **Context metrics** for each Session and the full Session tree, including archived descendants. Rows group by context strategy (`three-layer` or `summary`) and provider, model, effort, and Fast mode. **Export** includes the same measurements in JSON and Markdown. A recorded counter or setting is `measured`, a calculation or estimate is `derived`, and unavailable data is `unknown`. JSON uses a `null` value with source `unknown` for unavailable measurements. A known total can also show how many observations were missing.

| Measurement | What to compare |
| --- | --- |
| Boundaries | Counts by `threshold`, `over_budget`, `header_change`, `mid_turn`, and provider overflow. One rebuild may have several causes. Each rebuild shows the input before and after, system and tool header tokens, L2 copy and target tokens, rendered L1 tail tokens and items, W, and whether the required minimum exceeded its target. The report also gives the recent-event and excerpt settings, wall time, and observed requests between consecutive boundaries. Rollups average the known sizes and intervals. |
| Fallbacks | Each `fallback: l2_over_budget` shows the attempted required input, system and tool header, L2 copy and target, required L1 tail, and input budget. |
| Cache | The first request after each boundary and later requests have their own read, write, and uncached-input counters. Claude Code's main requests come from individual streams. The hit ratio divides the sum of cache-read tokens by the sum of total input for requests reporting both. The list-price equivalent uses the versioned price table and itemises uncached input, reads, writes, and output. |
| Summary comparison | CLI and Unreal compaction events show their outcome, recorded before/after sizes, summary output and generation time. Unreal's before size is the provider-reported summary request input, including the summarization instruction. The first post-compaction request input and the CLI's `post_tokens` are individual observations. |
| L3 | Per-window `record show` and `record find` calls and evidence bytes. Show counts page bytes and find counts excerpt bytes. The previous-tail re-read rate uses calls with a recorded membership check. A call counts once when any returned locator refers to the previous raw L1 projection. Byte and line selections refer to the same underlying evidence. |
| L2 | Recorded state decisions, plans, rules, acceptances, rejections, and question notes. Machine events are counted by kind using canonical replay. Boundary rows show unanswered user and child questions and pending approvals from the copied state or the state at compaction. |
| Child reviews | Grouped by the strategy at delegation and the child's model, with the child's strategy shown where known. The rejection-rate denominator is normally ended children with an explicit final accept or reject. Normally ended children without a review have their own count. |
| User questions | Visible assistant messages starting with `【要確認】` per work hour by strategy. Work time is the union of known regular-turn and tool intervals within each Session. Tree totals sum Session hours, including concurrent child work. |

Subscription utilisation shows provider-reported allowance windows, percentages, and reset times when available. Treat these as point-in-time account readings. The list-price table reports USD equivalents. [Codex rate-limit headers](https://github.com/openai/codex/blob/main/codex-rs/codex-api/src/rate_limits.rs) · [Claude Code rate-limit events](https://code.claude.com/docs/en/agent-sdk/typescript#sdkratelimitevent)

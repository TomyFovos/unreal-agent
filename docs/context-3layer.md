# Three-layer context

All providers and agent roles use a conversation window (L1), a copy of current state (L2), and locators for original evidence (L3). The three-layer design is tracked in [Issue #41](https://github.com/TomyFovos/unreal-agent/issues/41).

## Read canonical evidence

`record show LOCATOR` reads a locator. `record find TEXT` searches committed records in that Session and its descendants, including archived children. Their native tools are `RecordShow` and `RecordFind`.

- `rec:<session>/<sequence>` returns an exact JSONL item, including its line ending. Suffixes select the original user, peer, or assistant text, a recorded tool call, a child Finish, or a captured artifact. Child Finish locators include the child handle and can select `summary`, `tests`, `changed-files`, `blockers`, or a recorded `next` field.
- `rec:<session>/output/<operation>/<stream>` selects a terminal operation stream. Shell stdout and stderr come from their existing capture files. Remote jobs retain original bytes when their existing display value differs. Older previews whose originals are unavailable yield an explicit error. Standalone operation checkpoints also have locators for their exact JSONL bytes.
- `rec:<session>/<sequence>/tool-error` reads the original error text of a tool call that failed before starting an operation.
- `Session.RecordArtifact` explicitly captures a file version. The locator includes its path and SHA-256. Session-owned attachments preserve captured versions when the workspace copy changes or is removed.
- `?bytes=<start>-<end>` selects a zero-based half-open byte range. `?line=<number>` selects a one-based line with its original ending. The caller can request pages of a chosen byte size and follow the explicit continuation locator. Zero requests the entire selection.
- Find filters by kind, handle, inclusive start time, exclusive end time, and case-sensitive substring. Excerpts use the exact match, a first line, or a literal token from a structured record. Time filters require a recorded timestamp.

`record show` writes the selected bytes directly. `--json` and `--page-bytes` return text or base64 and the continuation locator when present. `record find` returns JSON matches and supports `--kind`, `--handle`, `--from` and `--before`. Times use RFC3339.

The processed counts in L2 link to `rec:<session>/context/<slot>`. The children index lists the Session's child handles, statuses, and canonical operation and report locators. Other indexes list source locators for the slot's evidence. When the L2 copy uses its token budget, it links to `rec:<session>/context/state` for the full recorded state and its evidence locators. Use `record find` to locate specific source records, including older versions.

The Host uses the Session's current read permissions for file-backed sources. Session history resolves through the same committed-log and archive readers used for inspection. Input receipts and observations carry source locators. A mid-turn continuation preserves the locators it offered, and a subsequent response still acknowledges delivery through the existing input watermark.

## Rebuild current state

L2 has thirteen slots. They are `session`, `requests`, `rules`, `plan`, `decisions`, `children`, `approvals`, `waiting`, `questions_to_user`, `artifacts`, `issues`, `recent_events`, and `metrics`. Entries carry source locators. Active human requests retain their original text in the copy when they fit. Mixed input retains its alias and provenance. Processed items keep a count and an L3 index locator.

S6 lists unfinished children and ended children whose review is pending. An explicit accept, reject, or cancellation settles a child. For an older child with no review, the projection settles it after the parent's request includes its terminal report and produces a recorded response. Delivery can come from a terminal peer notice, a child tool receipt, or a recorded show or wait result. A terminal operation checkpoint alone leaves the review pending. Children that ended during the current active human request keep their pending review even after the parent has seen the report. Settled children are counted by template, model, and ending. Explicit review statistics use the recorded reviews.

S10 lists every path reported by a child in S6 and every path the coordinator wrote since the last semantic boundary. Before the first boundary, coordinator writes are eligible from the start of the Session. A path shared by multiple writers appears once with its latest writer. Other paths contribute to the processed count.

`Session.AppendContextEvent` commits an explicit `context_state_event` to the same JSONL as the conversation. `Session.ContextState` returns a detached snapshot. The reducer applies each event ID once, and retrying an append returns the first event's locator. The restore checkpoint stores the reduced state and applied IDs. Full replay and checkpoint plus tail produce the same state bytes. A fork retains inherited events as evidence and starts with its own current state.

The writer and replay derive machine events from the same canonical inputs, responses, permission records, and operation checkpoints. A child start carries the recorded brief. Progress goes into recent events, together with observed routing rule IDs. Child completions carry the Finish locator when a report exists. Older checkpoints that embed a Finish can be read at the checkpoint locator. S7, S8, and S11 carry open approvals, waits, and issues. Resolutions, replacements, and child endings settle the corresponding entries. S9 carries visible `【要確認】` questions until the next human input or an explicit answer link. The next human input settles the L2 wait. The coordinator can still record which message answered the question.

Artifact paths come from changed files in Finish reports and successful write receipts. An explicit capture links a stored version of a file. File contents are available from that version or through the normal file read tools.

The log preserves unknown event versions and kinds for a future reader. The reducer applies the versions and kinds it understands. `state show` and `StateShow` read the latest L2 on demand.

## Rebuild the Responses request window

The first rebuild can occur as soon as the complete input reaches W, including the first request after upgrading a long-running Session. Each rebuild fixes the system instructions, tool schemas, and a copy of L2 with evidence locators. Within the L2 budget, active requests, approvals, questions, waits and running children take priority. Entries that fall outside the copy remain in L3. The raw tail starts at the last human message when that fits. To recover headroom, the Host advances the cut to the in-flight turn and then displaces any of its already-delivered batches as needed. It keeps undelivered inputs and unacknowledged tool call and receipt groups. The L1 head gives the locator range for earlier conversation in L3.

The rebuilt input targets `max(1, floor(PostRebuildRatio × min(W, input budget)))` tokens. Long tool outputs and assistant messages can be excerpted to reach the target. This fit preserves incoming messages verbatim. Excerpted items keep head and tail text with L3 locators, or an omission marker and locator at zero allowance. If the required minimum exceeds the target, it can use the remaining input budget. The hard input budget can require excerpts of incoming messages as well. Typed images retain their bodies. If the smallest projection still exceeds the input budget, the request fails before inference with the header, L2, and tail measurements.

After a rebuild, an estimated W tokens of growth triggers the next size-based rebuild. An input budget overflow or a header change can trigger one sooner. A provider overflow immediately rebuilds the window and retries the request once. Within a window, new entries append in recorded order. Large text tool results display a head and tail excerpt with locators for the original streams or receipt. Native image results retain their typed bodies. For a large source, `RecordShow` can return a byte range or page that fits the configured excerpt size.

The `window_boundary` record stores the causes, snapshot sequence, retained range, before and after input token counts, and whether the required minimum exceeded the target. The counts identify the estimator and whether the prior input used measured prefix usage. The first following Turn links to that boundary, and its response records cache read and write counts when reported. A fitted boundary records `Fit: excerpted`, the input budget, original required input and tail sizes, the fitted character allowance and the last unit it covers. Reopening from a restore checkpoint reuses the recorded L2 copy, tail and excerpt rules.

These keys are under `Context` in `runtime.json`.

| Key | Default | Purpose |
| --- | --- | --- |
| `WindowThresholdTokens` | `200000` | Positive W for the first rebuild and growth between rebuilds. |
| `PostRebuildRatio` | `0.5` | Fraction of W for the rebuilt input, strictly between 0 and 1. |
| `L2TargetTokens` | `5000` | Token budget for the state copy within the rebuild target. Source-backed entries that exceed the budget remain available in L3. |
| `RecentEventCount` | `0` | Use events since the previous boundary. At the first boundary, use events since the start of the active human request. A positive value selects the latest N for the copy. The full event history remains available. |
| `ToolOutputExcerptChars` | `0` | Use the existing tool display allowance. A positive value sets the combined head and tail size in Unicode characters for each text part. |
| `CacheRewarmMinutes` | `0` | Optional interval recorded for the PR7 cache comparison. Runtime requests remain driven by normal Session activity. |

Use `Context.Routes` to override these keys and `InputBudget`, `FallbackWindow`, `ResponseReserve`, or `ProtocolReserve` for specific provider IDs. A child first inherits the common settings, then applies the settings for the provider of its current selection, then applies `Context.Routes.<provider>.Roles.<role>` for its resolved role (`lead`, `worker`, or `collector`). The main Session uses the common and provider settings. Omitted fields inherit their previous values. Every effective policy must keep `L2TargetTokens` within the rebuild target computed from W and the ratio. The builder further caps L2 when the model's input budget or request header leaves less room.

For example, merge this `Context` object into the Host's runtime configuration.

```json
{
  "Context": {
    "WindowThresholdTokens": 200000,
    "PostRebuildRatio": 0.5,
    "L2TargetTokens": 5000,
    "Routes": {
      "claude": {"WindowThresholdTokens": 180000, "PostRebuildRatio": 0.6},
      "opencode-go": {
        "L2TargetTokens": 3500,
        "Roles": {
          "lead": {"WindowThresholdTokens": 64000},
          "worker": {"WindowThresholdTokens": 96000}
        }
      },
      "openai-codex": {"WindowThresholdTokens": 220000}
    }
  }
}
```

### Tune a running Host

Save the complete `runtime.json` used by the Host through an atomic file replacement. The Host and running children read `Context` before their next request and at Claude's tool batch checks. An invalid edit logs a warning and the process continues using the last valid settings. Resizing uses the new thresholds at the next check. The current projection is used until a rebuild is due. Each new Session records its initial policy with its first window and writes a `context_policy` event for each effective change. A Session resumed from an older log records its first applied policy at the next check.

Existing `runtime.json` files can still contain `Context.WindowStrategy`. Unreal ignores that key and logs one deprecation line on startup. The old Claude compaction thresholds are also accepted for compatibility.

## Rebuild the Claude Code window

The subscription and explicitly selected API-key routes use the same boundary conditions and tail selection. At a boundary, Unreal starts `claude -p` with a fresh `--session-id`. The CLI receives Unreal's usual appended system prompt and the new window's L2 copy and public L1 tail. Signed thinking remains in the old Claude transcript on disk.

The `provider_conversation_binding` record links the Unreal Session and window boundary to Claude Code's conversation ID. Between boundaries and after Host restart, `--resume` uses that ID and sends the new input. Unreal owns children, approval requests, unanswered questions and usage-limit waits.

At each main `PostToolBatch`, Unreal waits for approvals and records terminal tool receipts. It checks the latest model call's input tokens, including cache reads and writes. When a rebuild is due, Unreal includes queued inputs in the new window and records a boundary with the `mid_turn` cause. It returns `continue: false` with a `stopReason`, then starts a new conversation after the CLI ends the turn. Within a window, `PostToolBatch` sends queued inputs to the next call in the same conversation. A rebuild due after a final answer happens at the next request. [Claude Code PostToolBatch hook](https://code.claude.com/docs/en/hooks#posttoolbatch)

Admission estimates the rendered window text and the request's system and tool header. Subsequent turns can use the last reported model call's full input size with the estimated change in text. The first request's cache record uses the first main model call's counters, even if the CLI makes more model calls in the same turn.

Each Claude Code turn sets `DISABLE_AUTO_COMPACT=1` and `DISABLE_COMPACT=1` in the environment and launch settings. A `Prompt is too long` error from the CLI triggers the same one-retry window rebuild. [Claude Code environment variables](https://code.claude.com/docs/en/env-vars)

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

Open `/analyze` → **Context** to compare request input with W and cache reads before and after rebuilds. **Export** contains detailed measurements in JSON and Markdown for the full Session tree, including archived descendants. Rows group by context strategy (`three-layer` or `historical summary`) and provider, model, effort, and Fast mode. A recorded counter or setting is `measured`, a calculation or estimate is `derived`, and unavailable data is `unknown`. JSON uses a `null` value with source `unknown` for unavailable measurements. A known total can also show how many observations were missing.

The Context view shows the latest applied policy by route and role. Each effective change opens a new policy period for the affected Session. Compare the settings, rebuild counts, average input size just after rebuilding, cache read share on the first request, and L3 show and find calls. Window measurements belong to the policy that built the window, including L3 calls that begin before the next rebuild and complete afterward. Rebuilds from before policy recording appear with an unknown policy.

| Measurement | What to compare |
| --- | --- |
| Boundaries | Counts by `threshold`, `over_budget`, `header_change`, `mid_turn`, and provider overflow. One rebuild may have several causes. Each rebuild shows the input before and after, system and tool header tokens, L2 copy and target tokens, rendered L1 tail tokens and items, W, and whether the required minimum exceeded its target. The report also gives the recent-event and excerpt settings, wall time, and observed requests between consecutive boundaries. Rollups average the known sizes and intervals. |
| Excerpt fits | Each fitted boundary shows the input budget, required input and tail before fitting, final input and tail sizes, and the chosen allowance. |
| Cache | The first request after each boundary and later requests have their own read, write, and uncached-input counters. Claude Code's main requests come from individual streams. The hit ratio divides the sum of cache-read tokens by the sum of total input for requests reporting both. The list-price equivalent uses the versioned price table and itemises uncached input, reads, writes, and output. |
| Historical summary | Past CLI and Unreal compactions show their recorded outcome, input sizes, output usage and timing. Old fallback records retain their size breakdown. Session replay keeps the original log readable and rebuilds a three-layer window when it next uses a historical context. |
| L3 | Per-window `record show` and `record find` calls and evidence bytes. Show counts page bytes and find counts excerpt bytes. The previous-tail re-read rate uses calls with a recorded membership check. A call counts once when any returned locator refers to the previous raw L1 projection. Byte and line selections refer to the same underlying evidence. |
| L2 | Recorded state decisions, plans, rules, acceptances, rejections, and question notes. Machine events are counted by kind using canonical replay. Boundary rows show unanswered user and child questions and pending approvals from the copied state or the state at compaction. |
| Child reviews | Grouped by the strategy at delegation and the child's model, with the child's strategy shown where known. The rejection-rate denominator is normally ended children with an explicit final accept or reject. Normally ended children without a review have their own count. |
| User questions | Visible assistant messages starting with `【要確認】` per work hour by strategy. Work time is the union of known regular-turn and tool intervals within each Session. Tree totals sum Session hours, including concurrent child work. |

Subscription utilisation shows provider-reported allowance windows, percentages, and reset times when available. Treat these as point-in-time account readings. The list-price table reports USD equivalents. [Codex rate-limit headers](https://github.com/openai/codex/blob/main/codex-rs/codex-api/src/rate_limits.rs) · [Claude Code rate-limit events](https://code.claude.com/docs/en/agent-sdk/typescript#sdkratelimitevent)

### Replay a candidate offline

```bash
python3 scripts/context_replay.py ./session-copies --since 2026-11-24T00:00:00+09:00 --config ./candidate/runtime.json
```

Copy the Session logs before replaying. The `candidate` results include each route's effective policy, its configured `role_policies`, and simulations for the full trace and the validation period. Replay selects the role from the recorded child configuration or policy. Logs without a recorded role use the provider policy. Optional `--request-csv` uses the same candidate and includes the child role. The existing `codex` and `deepseek` report groups resolve policy from `openai-codex` and `opencode-go`, respectively. Candidate sizing applies the configured W, ratio, L2 cap, and budgets. The simulator estimates rendered context sizes, cache behaviour, and costs from recorded request metadata.

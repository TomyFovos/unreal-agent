# Three-layer context

The provider-neutral Host API provides raw evidence (L3) and the current state (L2). The three-layer design is tracked in [Issue #41](https://github.com/TomyFovos/unreal-agent/issues/41).

## Read canonical evidence

`record show LOCATOR` reads a locator. `record find TEXT` searches committed records in that Session and its descendants, including archived children. Their native tools are `RecordShow` and `RecordFind`.

- `rec:<session>/<sequence>` returns an exact JSONL item, including its line ending. Suffixes select the original user, peer, or assistant text, a recorded tool call, a child Finish, or a captured artifact. Child Finish locators include the child handle and can select `summary`, `tests`, `changed-files`, `blockers`, or a recorded `next` field.
- `rec:<session>/output/<operation>/<stream>` selects a terminal operation stream. Shell stdout and stderr come from their existing capture files. Remote jobs retain original bytes when their existing display value differs. Older previews whose originals are unavailable yield an explicit error. Standalone operation checkpoints also have locators for their exact JSONL bytes.
- `Session.RecordArtifact` explicitly captures a file version. The locator includes its path and SHA-256. Session-owned attachments preserve captured versions when the workspace copy changes or is removed.
- `?bytes=<start>-<end>` selects a zero-based half-open byte range. `?line=<number>` selects a one-based line with its original ending. The caller can request pages of a chosen byte size and follow the explicit continuation locator. Zero requests the entire selection.
- Find filters by kind, handle, inclusive start time, exclusive end time, and case-sensitive substring. Excerpts use the exact match, a first line, or a literal token from a structured record. Time filters require a recorded timestamp.

`record show` writes the selected bytes directly. `--json` and `--page-bytes` return text or base64 and the continuation locator when present. `record find` returns JSON matches and supports `--kind`, `--handle`, `--from` and `--before`. Times use RFC3339.

The Host uses the Session's current read permissions for file-backed sources. Session history resolves through the same committed-log and archive readers used for inspection. Input receipts and observations carry source locators. A mid-turn continuation preserves the locators it offered, and a subsequent response still acknowledges delivery through the existing input watermark.

## Rebuild current state

L2 has thirteen slots. They are `session`, `requests`, `rules`, `plan`, `decisions`, `children`, `approvals`, `waiting`, `questions_to_user`, `artifacts`, `issues`, `recent_events`, and `metrics`. Entries carry source locators. Active human requests carry the full original text, and mixed input retains its alias and provenance. A finished child awaiting review remains current. Settled items keep counts and locators, with child counts grouped by template, model, and ending. Current artifact paths belong to current children or to the coordinator's writes since the last semantic boundary.

`Session.AppendContextEvent` commits an explicit `context_state_event` to the same JSONL as the conversation. `Session.ContextState` returns a detached snapshot. The reducer applies each event ID once, and retrying an append returns the first event's locator. The restore checkpoint stores the reduced state and applied IDs. Full replay and checkpoint plus tail produce the same state bytes. A fork retains inherited events as evidence and starts with its own current state.

The writer and replay derive machine events from the same canonical inputs, responses, permission records, and operation checkpoints. A child start carries the recorded brief. Progress goes into recent events, together with observed routing rule IDs. Child completions carry the Finish locator when a report exists. Older checkpoints that embed a Finish can be read at the checkpoint locator. Approval requests and dispositions, operation and model waits, and forwarded child questions update the current state. User input has a verbatim locator. A line beginning with `【要確認】` opens a user question. The next input leaves that question open for the coordinator to match explicitly.

Artifact paths come from changed files in Finish reports, successful write receipts, and explicit file captures. These paths carry the evidence for the observation. File contents are available from a captured version or through the normal file read tools.

The log preserves unknown event versions and kinds for a future reader. The reducer applies the versions and kinds it understands. The model receives L2 when it calls `state show` or `StateShow`. Automatic projection belongs to PR5.

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

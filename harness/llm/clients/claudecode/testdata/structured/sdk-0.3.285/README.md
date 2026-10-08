# SDK 0.3.285 structural fixtures

SSOT: local `/tmp/unreal-claude-discovery-sdk/sdk.d.ts`, paired with `sdk.mjs` Version 0.3.285. These are synthetic shapes, not captured prompts or provider output. No real values, credentials, account data or reasoning are used. Fixture identities/private text are deliberate dummy sentinels for exclusion assertions.

- SDKAssistantMessage: type/message/parent_tool_use_id/uuid/session_id; no subtype. Optional request_id, user_message_uuid(s), resume_reason, supersedes, timestamp. Nullable parent; API stop_reason/stop_sequence can be null. Installed BetaMessage permits absent helper id and opaque model; neither is public authority.
- SDKPartialAssistantMessage: type/event/parent_tool_use_id/uuid/session_id. Optional ttft_ms and user/resume metadata. Nullable parent. Complete message follows partial events.
- SDKUserMessage: type/message/parent_tool_use_id. Optional synthetic/result/priority/timestamp/uuid/session_id metadata. Only receipts correlated to StructuredOutput are admitted by the isolated adapter.
- SDKControlResponse: type/response; success has subtype/request_id and optional response/pending arrays. Error has subtype/request_id/error. Pending permission/dialog requests cannot be approved by this adapter.
- SDKResultSuccess: type/subtype/durations/is_error/num_turns/result/stop_reason/cost/usage/modelUsage/permission_denials/uuid/session_id. Optional structured_output is REQUIRED by Unreal structured mode. Nullable stop_reason/api_error_status. Private result text and IDs are dropped.
- SDKResultError: closed error subtypes with errors[] instead of public result; never a completion/proposal.
- SDKAssistantMessageError: all 13 values are fixed in assistant_error_test.go and exercised without a CLI.

Tests mutate absent/nullable/wrong-type/security fields independently. Benign unused metadata is not promoted to canonical state; authority comes only from schema-validated result.structured_output. Nonempty parent/task/deferred execution is rejected. Distinct serialization retries may have multiple IDs; duplicate IDs within one assistant content list are malformed.

`unknown-metadata.jsonl` is a **negative, unresolved shape fixture**, not a newly
supported SDK frame. It reproduces the observed wrapper-object/message-array
counts, the known nullable fields, one StructuredOutput block and partial
input_json_delta using dummy values. The actual unrecognized field names were
not present in the supplied diagnostic. A literal `unknown_fields` name and
different arbitrary names produce the same old aggregate display; neither
proves that `unknown_fields` is a producer field. The local 0.3.285 declarations
and installed 2.1.285 assistant schema do not declare that literal name.

Unknown metadata remains fail closed as a serializer-frame protocol error.
It is not evidence that a tool executed. Actual Bash/Read/Edit/Agent/Web/MCP
execution still takes the execution rejection path. Diagnostics report the
rejection scope, a declared CLI field name or `unknown` for assistant metadata,
and whether the literal `unknown_fields` key is actually present. A rejected
message member name is additionally shown exactly when it is 1..64 ASCII bytes
from `[A-Za-z0-9_.-]`; otherwise it is `unknown`. This is an observability rule,
not field acceptance. Values and raw metadata bodies are never retained.

`wire-tool-inputs.jsonl` adds the installed CLI's optional top-level
`wire_tool_inputs` record. Its values are raw API input objects keyed by the
checked StructuredOutput block IDs in the same assistant frame. They may differ
from normalized helper input and from the terminal structured_output. The fixture
deliberately uses different dummy values and includes partial input_json_delta;
only the terminal public completion is authoritative.

`wire-tool-inputs-message-unknown.jsonl` retains an unrecognized array inside
message under a dummy name. It must still reject with scope=message; the actual
producer name has not been established. This is not a message allowlist addition.

The transport-only exception accepts absent/empty/object records within 1 MiB,
64 entries, 32 nested levels and 4096 JSON values. Null, non-object entries,
uncorrelated IDs, malformed/duplicate JSON, execution-shaped nested objects and
unknown sibling fields remain rejected. Values are discarded and never exported
to diagnostics, public conversation, canonical history or Operation creation.

`input-transformations.jsonl` adds the observed message-level
`input_transformations` field with the installed consumer's checked entry shape:
an optional array of `{type: "thinking_dropped", path: string, reason: string}`.
The bundle accumulates non-null message_delta input transformations onto the
Messages API message; this is general assistant input/thinking metadata, not a
StructuredOutput-only feature or an execution proposal. The public SDK wrapper
references BetaMessage and does not declare this field explicitly.

The isolated profile admits absent/empty arrays or at most 64 entries in 1 MiB.
Each entry has exactly the three known scalar fields, so container depth is at
most 2 and JSON value count at most 257. Present null, unknown transformation
types, extra nested fields, execution objects and malformed JSON remain rejected.
The native stream accumulator skips null delta values; no broader nullable
message-field contract is assumed. Path/reason strings remain opaque and are
discarded even when they contain private data or JSON-like text. They never
enter diagnostics, canonical history, Permission, Registry or Operation state.
The fixture coexists with wire_tool_inputs, StructuredOutput and partial
input_json_delta. The final result.structured_output remains the sole authority.

`probe-provider-unknown.jsonl` is a text-only assistant error frame with the
observed public SDK `error: unknown`. IDs, model, timestamp and prose are dummy
sentinels. The optional boolean spelling `is_api_error_message` is taken from
the installed CLI's API-error serializer, not a captured real payload: the
operator's past `unrecognized_field_types:boolean(1)` name remains unconfirmed.
The fixture must classify as provider_failure / assistant_unknown before
serializer/result/schema validation, with no proposal, public response, MCP
execution or Operation. No metadata acceptance is added. Error diagnostics
may expose one unrecognized assistant/message key using the 1..64 ASCII
`[A-Za-z0-9_.-]` rule; values and unsafe names remain excluded.

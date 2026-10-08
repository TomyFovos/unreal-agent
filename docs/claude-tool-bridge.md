# Claude Code → Unreal Agent Tool Bridge v1

## Bridge modes

| Configuration | Protocol | Execution owner |
| --- | --- | --- |
| `Enabled: false` | Text only | No tool calls |
| `Enabled: true, Mode: "structured"` | Action/Final structured output; a new generation after each receipt | Unreal Registry / Permission / Operation |
| `Enabled: true, Mode: "mcp"` | Official SDK MCP compatibility bridge | Unreal Registry / Permission / Operation |

Missing `Mode` on an existing enabled bridge preserves **MCP** behavior. New
normal-launcher bootstrap selects `structured` explicitly. There is no automatic
MCP-to-structured, built-in-tool, API-key or provider fallback. Unknown modes
fail configuration validation. Existing immutable Session configurations are
never rewritten; choose a new Session when changing its creation configuration.

## Structured Action Bridge v1

Claude Code **2.1.285** advertises `--json-schema`. The official
[structured-output contract](https://code.claude.com/docs/en/agent-sdk/structured-outputs)
uses `outputFormat: {type: "json_schema", schema: ...}` and returns validated
`result.structured_output`. The matching
[0.3.285 SDK types](https://unpkg.com/@anthropic-ai/claude-agent-sdk@0.3.285/sdk.d.ts)
define `initialize.jsonSchema`; Unreal sends this schema over stdin rather than
putting schema/prompt contents in argv. No Node/Python SDK runtime is required.

The installed CLI creates the internal **StructuredOutput** serializer. Its
implementation is read-only, concurrency-safe, not open-world, and not MCP. It
validates its JSON input, returns `structured_output`, and ends the generation;
its call implementation contains no filesystem, shell, child or network executor.
This is the sole permitted Claude internal tool and **creates no Unreal Operation**.
Unreal independently revalidates its result with Draft 7 JSON Schema and rejects
duplicate keys, invalid UTF-8, contradictory envelopes, unknown tools, malformed
arguments and output exceeding 1 MiB. External schema resource loaders are disabled.
Intermediate assistant prose and private reasoning are discarded, never executed
or published. Only `final.message` becomes a public agent response.

```text
bounded Context Package + private Action/Final instructions
  → isolated Claude generation
  → StructuredOutput serialization
  → result.structured_output
  → independent Unreal schema / registry validation
  → llm.ToolCall (one action)
  → existing Translator / Permission / canonical Operation / Executor
  → canonical terminal receipt
  → new bounded Context Package
  → next independent Claude generation, or public Final
```

Unreal's authoritative schema is a discriminated union of
`{type:"action", action:{id,tool,arguments}}` and `{type:"final", final:{message}}`,
with exactly one branch and no extra members. The provider receives a separate
closed transport object, with a type enum, optional action/final properties,
declared tool enum and a closed superset of registered argument properties.
It contains no root oneOf or per-tool union. Both schemas derive from the same
immutable Registry snapshot. Tool names, descriptions and original argument
schemas also enter private protocol instructions; they are not canonical
conversation or an authorization grant. There is no second filesystem, patch,
shell or child engine. `SubagentStart` keeps its existing arguments:
`{template, task, runtime:{provider,model,effort}}`. Unreal resolves the runtime,
limits children and permissions, and inherits the parent's bound Project
Instructions snapshot. Models never launch another provider directly.

Structured launch retains `--safe-mode`, `--restricted`, `--tools ""`, strict
**empty** MCP, disabled settings sources/slash commands/Chrome, private system
prompt transport, `--permission-prompts none`, `--no-session-persistence`, and
`--max-turns 5`. This bounded internal allowance matches Claude Code 2.1.285's
five-attempt StructuredOutput correction limit. Helper retries never create
Unreal ToolCalls or Operations: only a successful, strictly revalidated terminal
`result.structured_output` can do so. `error_max_turns` becomes the typed
`structured_serializer_round_limit` failure, with no accepted Action or Final.
The exact allowlist is **StructuredOutput only**; the named deny
list blocks side-effecting built-ins and `mcp__*`. Stream init may list only this
serializer (or an empty available list), must attest to `mcp_servers=[]`, and
must keep `permissionMode=default`. All other actual tools/MCP/tasks/agents and
uncorrelated execution events remain fatal. No `--bare` or `CLAUDE_CODE_SIMPLE`
is used; Team OAuth remains owned by Claude CLI. Managed administrative behavior
remains outside Unreal's boundary only under explicit `managedPolicyMode=trust`.

Claude Code 2.1.285 also emits a fixed `isSynthetic=true` user reminder when
an assistant finishes without calling StructuredOutput (`requiresStructuredOutput`
enforcement, installed bundle byte 210935199; serializer byte 210298490;
SDK 0.3.285 `SDKUserMessage.isSynthetic`, `sdk.d.ts:6165`). Structured mode
discards only that exact bounded reminder and its known envelope, including
the exact text/string-or-single-text-block representation. It cannot become
a user input, receipt, Action, or public response. Unknown/mixed user content,
execution metadata, replay, and reminders after the final result are rejected.
This exception never interprets free text as work; the terminal structured result
still passes the unchanged strict Host schema, Registry, Permission and Operation.

The [official partial-event flow](https://platform.claude.com/docs/en/build-with-claude/streaming)
ends with `message_delta`/`message_stop` before the complete assistant frame.
Serializer-only tool stops are correlated with the checked `StructuredOutput`
start event; an uncorrelated tool stop is rejected. Aggregate partial JSON is
size-bounded. The **authoritative proposal is `result.structured_output`**, after
Unreal's independent envelope, registry and complete JSON Schema validation.
The public SDK does not promise exact equality with internal assistant helper
input, partial JSON or a previous failed serializer attempt. Those frames attest
only to the permitted serializer, never request execution themselves. A helper
input must still be a well-formed, bounded JSON object; duplicate keys and all
non-serializer tool use remain fatal.

`SDKAssistantMessage` is discriminated by `type: "assistant"`; the pinned
0.3.285 type declares **no assistant subtype**. Its nested `BetaMessage` carries
`content`, model and nullable stop metadata. The installed 2.1.285 wire schema
also permits an absent `message.id` and an opaque string model; local messages
can carry `<synthetic>`. These helper identities are private metadata, not
authorization. Unreal validates a message object and content array, well-formed
text/thinking blocks and object-valued `StructuredOutput` input. No subtype is
required. Side-effecting tools, unknown tool names/blocks, non-direct callers,
container/server-tool execution and unrecognized execution metadata are rejected.
Correlated serializer display labels and the SDK's optional private identifiers,
timestamps and nullable stop fields are discarded. A synthetic/opaque helper
model cannot replace the validated init model observation.

The Messages API allows multiple content blocks and the SDK allows serializer
retries: there is no public one-helper-block guarantee. Several serializer
blocks still produce **one** authoritative terminal Action/Final, never several
Unreal actions. No helper frame itself creates an Operation.

Pinned SDK `tool_progress` / `tool_use_summary` are accepted only when every
identity correlates to a checked StructuredOutput helper, with no task/subagent
execution markers and the supported metadata shape. Their text is discarded.
The SDK transport filters `keep_alive`; Unreal accepts only its exact heartbeat
shape, including after the result. Every other trailing frame, including a tool
or control frame, fails **before** the Action reaches the Host. Unknown event
shapes remain fail-closed.
An execution-linked `tool_use_result` on a terminal result is also rejected;
serializer receipts belong only to the correlated SDK user/tool-result frames.

Installed 2.1.285 compiles the supplied schema with Ajv, then separately derives
an optional strict-sampling schema. Its supported derivation excludes oneOf.
Real Opus returned an upstream error even for a single root oneOf around an
otherwise passing Final object. OneOf-free T1–T7 schemas passed live. This is
the observed compatibility boundary; the opaque upstream error is not proof
of a universal keyword ban. Production now supplies the compatible transport.
Unreal still keeps its Draft 7 oneOf, constants, ID pattern, closed envelopes
and original Registry argument constraints. In addition, undeclared root
argument names reject even for legacy tool schemas which omitted
additionalProperties. Null branches, both/neither branches, unknown types,
invalid IDs and another tool's arguments cannot become a ToolCall, permission
request, Operation or public response. Transport validity grants no authority.
The [API structured-output subset](https://platform.claude.com/docs/en/build-with-claude/structured-outputs#json-schema-limitations)
is not a reason to treat an SDK initialization acknowledgment as proof of
generation-time compatibility. Actual generation is checked by the result and
by Unreal's complete local validator.

### Safe structured-stream diagnostics

Every structured protocol failure has a closed stage at the start of its error:

```text
stage=structured_schema_rejected: Claude structured action protocol validation failed
```

| Stage | Failure boundary |
| --- | --- |
| `structured_init_frame_invalid` | Malformed/rejected initialize response or invalid init isolation attestation |
| `structured_unexpected_preinit_frame` | Unexpected frame before the initialize acknowledgment |
| `structured_init_incomplete` | Initialization ended without its acknowledgment |
| `structured_serializer_frame_invalid` | Invalid complete helper frame/input object |
| `structured_serializer_partial_invalid` | Invalid partial-event shape/index/order |
| `structured_result_invalid` | Invalid or unsuccessful terminal result |
| `structured_result_missing_output` / `structured_result_null_output` | Required public output absent/null |
| `structured_json_decode_failed` | Invalid JSON or UTF-8 |
| `structured_duplicate_fields` / `structured_trailing_json` | Duplicate members or more than one JSON value |
| `structured_action_envelope_invalid` | Ambiguous/incomplete/wrongly typed Action/Final envelope |
| `structured_schema_rejected` | Complete Host schema rejection or provider structured-output retry limit |
| `structured_unknown_tool` / `structured_arguments_schema_rejected` | Unknown registry tool or invalid arguments |
| `structured_response_oversized` | Output, serializer input, frame or total stream exceeds its bound |
| `structured_unexpected_control_frame` | Control/permission callback not belonging to this protocol |
| `structured_trailing_frame` / `structured_stream_incomplete` | Extra non-heartbeat frame, missing terminal result or unfinished helper block |
| `structured_execution_rejected` | Side-effecting built-in, MCP, task/subagent, uncorrelated result/progress/summary or other execution |
| `structured_system_frame_invalid` / `structured_stream_frame_invalid` | Unsupported/malformed system or other frame |
| `structured_permission_denied` | Recognized provider denial; remains typed `structured_unavailable` |
| `structured_usage_invalid` | Invalid accumulated usage, including overflow |

Errors also retain only safe schema metadata: allowlisted event/subtype and
top-level field names/types, closed content-block types, serializer count,
presence/null flags, schema-check flags and byte counts. Unknown keys/enums
become type/count or `unknown` because a key itself may contain protected data.
Partial-event/delta names also use closed enums. Accessor results are disposable
copies. Raw wire JSON, serializer/action values,
arguments/paths, prose, reasoning, decoder/schema error text, session/tool IDs,
accounts, policy bodies and credentials are never retained in diagnostics.
The stage is visible in the normal TUI failure without adding protocol JSON to
public conversation/history or analysis exports. Merely receiving a helper
frame creates **zero** ToolCalls/Operations/receipts.

Assistant failures additionally expose a closed `reason=assistant_...`, message
field names/types, content-block count and tool kind enum (`StructuredOutput`,
`side_effecting_builtin`, `foreign_mcp`, `unknown`). Absent assistant subtype
prints `subtype=unknown subtype_present=false`; **unknown is a diagnostic enum,
not evidence of an invalid assistant subtype**. Reasons distinguish message,
ID/model, content/block, serializer input, metadata and decode failures. Raw
message IDs/models, tool names outside that enum and unknown JSON key names are
never copied into diagnostics. The observed `structured_serializer_frame_invalid`
alone cannot distinguish the old decode, public-ID/model check and input-object
branches. Fixtures reproduce the confirmed optional-ID/opaque-model contract
mismatch; a real failure without its message shape must not be attributed to
missing subtype. The next smoke supplies the reason without recording values.

### Assistant provider errors (pinned 0.3.285)

The local reference `/tmp/unreal-claude-discovery-sdk/sdk.d.ts`, paired with
`sdk.mjs` whose version is **0.3.285**, declares the following 13
`SDKAssistantMessageError` values. They also match the installed 2.1.285 schema.
The adapter's versioned enum is based on that local pin, not another web version.
There is no runtime SDK installation, account or network dependency for these
offline classification tests.

| SDK value | Safe reason | Public typed category |
| --- | --- | --- |
| `authentication_failed` | `assistant_authentication_failed` | `external_reauth_required` |
| `oauth_org_not_allowed` | `assistant_oauth_org_not_allowed` | `subscription_unavailable` |
| `account_on_hold` | `assistant_account_on_hold` | `subscription_unavailable` |
| `verification_required` | `assistant_verification_required` | `subscription_unavailable` |
| `billing_error` | `assistant_billing_error` | `subscription_unavailable` |
| `rate_limit` | `assistant_rate_limit` | `rate_limited` |
| `overloaded` | `assistant_overloaded` | `subprocess_failure` (provider overloaded) |
| `invalid_request` | `assistant_invalid_request` | `provider_request_rejected` |
| `model_not_found` | `assistant_model_not_found` | `invalid_model` |
| `server_error` | `assistant_server_error` | `subprocess_failure` (provider server error) |
| `unknown` | `assistant_unknown` | `subprocess_failure` |
| `max_output_tokens` | `assistant_max_output_tokens` | `generation_output_limit` |
| `cloud_credential_error` | `assistant_cloud_credential_error` | `external_reauth_required` |
| Any non-SDK string | `assistant_provider_error_unknown` | `subprocess_failure`, fail closed |

`assistant.error` uses exact equality: no trimming, case folding, substring or
classification of assistant prose. Only a private numeric enum is stored in
Error; public reasons are reconstructed from compile-time constants. A malformed
error field remains a protocol failure. The legacy classifier remains for
result/errors/message failures, while SDK assistant errors use this closed
parser in structured, text-only and optional MCP compatibility modes.

Structured errors retain `stage=structured_result_invalid`, for example:

```text
stage=structured_result_invalid: Claude Code rejected the provider request
(reason=assistant_invalid_request ...)
```

This does not identify the request's underlying rejection detail. If the next
smoke yields `assistant_invalid_request`, investigate request/schema compatibility
without logging its body. `assistant_max_output_tokens` identifies incomplete
generation, not a malformed Action; investigate output/token handling.
`assistant_overloaded` and `assistant_server_error` identify provider failures:
an explicit user retry is possible, but the adapter never retries or routes to
another provider automatically. Account restrictions/verification stay distinct
from credential refresh via safe reasons. Cloud credential recovery belongs to
the external CLI; it never permits an API-key or alternative-routing fallback.
Every provider error before an Action leaves Operation/public response counts
at zero. Raw provider error text/prose, IDs and account data are excluded from
errors, canonical history and logs.

日本語: 正式な実行提案は最終 `result.structured_output` です。内部 serializer の
input との完全一致を公開契約とは扱いません。Unreal が JSON/envelope/schema、
Registry、Permission を再検証してから既存 Operation で実行します。エラーは
先頭の `stage=...` と値を含まない schema metadata だけを保持します。今回の
実 Opus の汎用エラーだけでは失敗分岐を断定できません。次回の stage を取得し、
本文や private 値を保存せず dummy fixture でその構造を再現してください。

A read-only probe against the installed Team CLI, with the production structured
launch contract, accepted `initialize.jsonSchema`: **zero user messages, zero
model generations, zero tool execution, zero MCP servers**. This confirms
feature/schema/authentication compatibility, not generation-time authorization.
Structured mode does not register SDK MCP, list MCP tools or request exact MCP
permission grants. If organization policy denies StructuredOutput itself,
`structured_unavailable` stops the request without fallback. Normal registration
keeps a denied provider text-capable with a visible `blocked_by_policy` status.

### Layered validation before any real smoke

Protocol debugging now follows [Structured Bridge layered validation](claude-structured-testing.md).
Schema/request/control/assistant/partial/serializer/result/error/envelope/arguments,
Registry/Permission/Operation/context/Final/recovery boundaries are tested without
real providers, then fake CLI/Host integration runs. `make test-claude-structured`
enforces the unit-before-integration gate.

Ordinary tests and full validation never enable real inference. Authorized
development uses focused offline/fake gates followed by short, explicitly
selected adapter probes. The old oneOf schema ladder is preserved for diagnosis;
the compatible T1–T7 transport path replaces it as the live compatibility gate.
The separate opt-in Action/receipt/Final test uses only read in a temporary
workspace and its own disposable Host/state/socket. It does not use AIdea or
existing Sessions. All offline gates and this real round gate must pass before
stopping for the AIdea E2E decision; do not run AIdea after individual fixes.
Existing failed Session history is retained; no configuration/history migration
or blind action replay is needed. Safe stage/reason diagnostics remain available;
raw private stream capture is unnecessary.

```json
"ClaudeCode": {
  "managedPolicyMode": "trust",
  "ToolBridge": {
    "Enabled": true,
    "Mode": "structured",
    "MaxActionRounds": 32,
    "TimeoutMillis": 300000,
    "ResultBytes": 65536
  }
}
```

Merge this into the active `Runtime.ClaudeCode`, registered
`Providers["claude-code"].ClaudeCode`, or `Launcher.ClaudeCode` for additional
normal discovery, according to existing precedence. Old enabled configurations
with no mode remain MCP; they are not silently converted. Disabled configurations
remain text-only. Bridge enablement **does not grant execution permissions**.
Normal defaults still grant only rooted `read`, `grep`, `glob`.

`MaxActionRounds` defaults to 32 (maximum 128). Each Action is sequential and
starts at most one Registry call. Replays also consume the generation bound.
Timeouts/cancellation propagate to the process group and existing Operation
manager; uncertain work is recovered through canonical Operations, never
speculatively retried. Each generation is independently reaped, including children
holding stdout pipes. No private continuation is needed or persisted.

Action IDs are local to a canonical input/task. Unreal namespaces them using that
input's hash. The same ID/payload reuses its receipt; a changed payload is a typed
conflict. A disposable replay index is rebuilt from canonical ToolCalls/receipts
on restart; a completed Operation is not executed again, and an uncertain one
requires recovery. A new explicit task has a new scope. The public Action envelope
is translated to the existing canonical ToolCall, rather than stored as protocol JSON.

The Context Engine rebuilds after receipts, preserving the current user task,
Project Instructions, runtime revision, recent raw context and retrieval. It
reserves space for private protocol framing, the Registry contract and the
latest machine-owned receipt.
Receipt output is bounded by `ResultBytes` and one quarter of the input budget,
with explicit head/tail truncation metadata. The full canonical receipt remains
unchanged. Protected output is withheld. Newly queued user/peer messages wait
for the logical turn's final boundary; provider/model/effort never switch mid-loop.
`/analyze Overview` exposes the closed bridge mode and `/analyze Context` shows
its estimated transport/current-receipt reserve. Orchestration reuses ordinary
canonical Operations/children. Conversation export includes Final text, never
serializer/action JSON, private state or raw receipts.

The transport supports the current Registry's scalar, array and closed-object
arguments. Unsupported references/unions/open dictionaries, missing object
properties or incompatible types for a shared argument property fail schema
construction. They do not trigger open additionalProperties or another tool
protocol. Adding such a future tool requires an explicit transport extension.

日本語: Structured mode では Claude は Action を提案するだけです。唯一許可する
Claude internal tool は出力用 `StructuredOutput` で、schema 検証・serialization
以外の副作用はありません。filesystem / shell / edit / search / child の実行は
すべて既存 Unreal Registry・Permission・Operation を通ります。MCP grant は不要で、
組織が serializer 自体を拒否した場合は typed unavailable として停止します。
free text を command と解釈せず、Unreal 側でも schema を再検証します。
`managedPolicyMode=trust` は組織管理者 policy を外部の信頼境界としてユーザーが
明示的に受け入れた指定であり、Unreal が policy の安全性を検証したという意味ではありません。

## AIdea manual preparation (not run by automated validation)

Use the existing normal configuration at `~/.config/unreal-agent/runtime.json`.
Keep the complete configuration and provider registrations; merge only reviewed
settings. Set `Runtime.Workspace` to `/home/e230038/src/unreal-agent/AIdea` for a
**new** Session and choose structured mode as above. Confirm `Launcher.Version=1`
so the isolated Claude transport is authorized independently of command permissions.
Remove `Bash` from `Runtime.DisallowedTools` only if you explicitly grant commands.
If another normal Host is running with different immutable configuration, gracefully
stop that verified Host with SIGTERM before relaunching; preserve state/history.
An existing `aidea` Session with a different creation identity needs a new name,
for example `aidea-structured-v1`; do not delete its history.

Rooted read/edit profile (no commands, no permission expansion from the bridge):

```json
"Permissions": {
  "Tools": ["read", "grep", "glob", "write", "edit"],
  "ReadRoots": ["/home/e230038/src/unreal-agent/AIdea"],
  "WriteRoots": ["/home/e230038/src/unreal-agent/AIdea"]
}
```

For test/build/git commands, the current executor supplies **no OS process sandbox**.
A reviewed development profile with `Bash` therefore explicitly accepts ambient
filesystem/network/process access; this is not a normal default or a bridge grant:

```json
"Permissions": {
  "Tools": ["read", "grep", "glob", "write", "edit", "Bash", "SubagentStart", "SubagentSend", "SubagentCancel", "Finish"],
  "ReadRoots": ["/home/e230038/src/unreal-agent/AIdea"],
  "WriteRoots": ["/home/e230038/src/unreal-agent/AIdea"],
  "ProcessMode": "unrestricted",
  "FilesystemUnrestricted": true,
  "NetworkUnrestricted": true
}
```

Review this broad command permission deliberately. Existing permission semantics
support concrete allow/deny, not a new human `ask` workflow or per-Git-command
approval. Configure existing child templates separately with permissions no
greater than this parent. No tokens, account identity, managed policy body or
parent environment belongs in this config. Keep Codex transport origins and
other required existing configuration fields when merging.

```sh
cd /home/e230038/src/unreal-agent/AIdea
unreal aidea
```

Choose `/model` → Claude Code → the discovered Opus model → effort. Start with
read-only inspection of AGENTS.md/source and `/view orchestration`; then supply
the reviewed development task, observe failed test receipt → edit → retest → git
diff, and inspect `/analyze Overview`, `/analyze Context`, `/export last`. Detach
with Ctrl-D and reconnect with `unreal aidea`. Only the user performs this real
inference smoke; automated tests use fake processes and temporary workspaces.

Known v1 limits: one Action per generation, text tool receipts only, 1 MiB structured
response limit, bounded replay generations, byte-based conservative token estimates,
and process startup cost per action. Schema acceptance cannot prove future policy
permission or real model adherence. Managed hooks/env/telemetry remain the explicitly
trusted administrative boundary; they are not execution authority granted to Claude
work tools. No auto routing, provider fallback, LLM summary or vector dependency is added.

## MCP compatibility: official transport and execution ownership

The implementation uses the official Agent SDK's **in-process SDK MCP server**
over Claude Code's bidirectional stream-JSON control transport. It implements
the public SDK protocol in Go; it does not require a Node/Python SDK dependency.
The [custom-tool contract](https://code.claude.com/docs/en/agent-sdk/custom-tools)
and [0.3.285 SDK types](https://unpkg.com/@anthropic-ai/claude-agent-sdk@0.3.285/sdk.d.ts)
define `sdkMcpServers`, `mcp_message` and SDK server provenance. A non-inference
probe against installed **2.1.285** established registration, MCP initialize,
tools/list and `mcp_status` with `source: sdk`, while retaining safe mode. No user
message or tools/call was sent by that probe.

```text
Claude structured SDK MCP request
  → Go protocol adapter (no executor)
  → provider-neutral Coordinator rendezvous
  → public ModelResponse / llm.ToolCall committed
  → existing Registry / Translator / permission checks
  → existing canonical Operation / Manager / executor
  → terminal canonical receipt
  → bounded SDK MCP result
  → Claude continuation in the same request process
```

The adapter generates `unreal_<RegistryName>` schemas directly from the existing
Registry. Claude sees names such as `mcp__unreal__unreal_read`,
`mcp__unreal__unreal_edit`, `mcp__unreal__unreal_Bash`, and
`mcp__unreal__unreal_SubagentStart`. These names denote **Unreal** implementations,
including the existing mutation revision checks and bounded reads/searches.
The MCP handler queues work to the Coordinator; it never opens a workspace file,
launches a command, translates an Operation or starts a child itself.

The preserved launch flags include `--safe-mode`, `--restricted`,
`--setting-sources ""`, isolation settings, `--tools ""`,
`--strict-mcp-config`, disabled slash commands/Chrome, disabled session persistence
and `--permission-prompts none`. The opt-in invocation replaces the old MCP-wide
`--disallowedTools "*"` with a named built-in deny list **and** an exact generated
`--allowedTools` list. `--tools ""` still removes **all** built-ins; an allowlist
does not restore them. The sole explicit MCP configuration is
`{"mcpServers":{"unreal":{"type":"sdk","name":"unreal"}}}`.
No stdio/HTTP MCP subprocess or arbitrary project server is launched.

Before sending user input, the adapter requires tools/list completion and a
single connected server with the exact SDK provenance. A managed/plugin server
with the same name, another MCP server, changed launch flags or an unknown
execution event fails closed. Init may advertise only generated Unreal names.
Permission mode must remain `default`. Native Bash/Read/Edit, server tool use,
foreign MCP, uncorrelated progress/results and Claude task/subagent execution
are rejected. Catalog plugins/skills/agents remain non-executable metadata.

### Permission denials and managed-only rules

SDK registration is not permission approval. Before sending any user input, the
same Claude process now performs the official read-only `list_permission_rules`
control request. Every generated SDK MCP tool must have an effective **exact
bare-name** grant from `cliArg` or `policySettings`; when `managedOnly` is true,
only the latter qualifies. This is a conservative readiness check, not a policy
override or a guarantee against later deny/ask rules, hooks or policy refresh.
Wildcard grants are not used to satisfy this check. No managed settings file,
credential file or policy body is opened by Unreal. Rule bodies stay local to
the parser; errors expose only expected/allowed counts and a fixed summary.

The non-inference check of installed **2.1.285** in the Team environment found
`managedOnly=true`, no effective exact grants for the supplied Unreal SDK tools,
and zero settings parse errors. Its installed policy-refresh implementation
removes `cliArg`/`session` **allow** rules under
`allowManagedPermissionRulesOnly`, while retaining denies. Thus the generated
`--allowedTools` names can match tools/list perfectly and still be inactive.
With `--permission-prompts none`, a remaining ask is terminally denied. This
explains the observed permission failure without changing safe mode, enabling
native tools or treating `managedPolicyMode=trust` as permission bypass.

An organization administrator must authorize the exact generated tools in
managed `permissions.allow` if that policy is intentional, for example
`mcp__unreal__unreal_read`, `mcp__unreal__unreal_grep`,
`mcp__unreal__unreal_glob`, `mcp__unreal__unreal_edit`,
`mcp__unreal__unreal_write` and `mcp__unreal__unreal_Bash`, **plus each other
schema exposed by that configuration** (AST, image, child, LSP/DAP, etc.). These
are Unreal implementations, not built-in `Read`, `Edit` or `Bash`. The generated
schema/argv equality is verified in fixtures; the adapter never adds a wildcard
allow, changes managed policy, or grants permissions to the Unreal executor.
The organization may still deny particular actions. Without effective grants,
the adapter returns typed `bridge_allowlist_inactive` **before user input**.

The official `SDKPermissionDeniedMessage` (`system/permission_denied`) is an
advisory notification of an auto-denied **tool call**, not a registration event.
Its `message` is a string, unlike assistant/user message objects. It may also
contain `tool_name`, `tool_use_id`, `agent_id`, `decision_reason_type`,
`decision_reason`, `uuid` and `session_id`; installed 2.1.285 also emits optional
`decision_reason_code`. The SDK's `result.permission_denials` is authoritative;
an advisory frame alone is not a canonical Unreal receipt. See the pinned
[public SDK schema](https://unpkg.com/@anthropic-ai/claude-agent-sdk@0.3.285/sdk.d.ts)
and [official permission evaluation](https://code.claude.com/docs/en/agent-sdk/permissions).

Both transports recognize this shape before decoding assistant messages:
text-only returns `permission_denied`, and the bridge returns
`bridge_permission_denied`. Neither continues to successful final text, invents
an Operation, retries a side effect, enables built-ins or switches providers.
Only a closed reason-type enum and an exact-owned-tool/unknown scope are retained.
Unknown future reason strings become `unknown`. Backend `message` and
`decision_reason` prose, private IDs, account information and extra fields are
discarded; UI uses a fixed safe summary rather than trying to redact arbitrary
policy text. Malformed known denials return `permission_denied_invalid_shape`;
truly unknown system events still fail through `system_unknown_shape`.

A provider denial is separate from Unreal's existing Translator/Permission
denial: the latter still produces the normal safe failure receipt and lets
Claude continue. SDK denials lack the original arguments and Unreal decision
needed to create such a receipt, so v1 fails the request closed. Orchestration
shows `provider permission denied`. `/analyze Errors` continues to count only
canonical errors; a transient Host/provider error is not fabricated into history.

日本語: `managedPolicyMode=trust` は組織 policy を信頼する指定であり、managed
permission rules の迂回や tool の自動許可ではありません。managed-only 設定で
CLI の allow rules が除去される場合は、組織管理者による exact Unreal SDK tool
名の許可が必要です。許可が無ければ入力送信前に停止します。
`permission_denied` は既知の typed failure として処理し、SDK の本文・private
ID・policy 内容は保存・表示しません。既存 history と Permission semantics は
維持します。

### Re-test the existing failed Team smoke Session

Keep `~/.config/unreal-agent/claude-tool-bridge-smoke.json`, its workspace and
state directory unchanged. An external administrative rule update does not
change the Session's immutable startup identity; no new Session is necessary.
Arrange the exact managed grants first. The local readiness check is not a
certificate that a future policy refresh cannot deny a call.

Detach with Ctrl-D and stop the **old Host** before using the newly built binary.
If it is foreground `serve`, use Ctrl-C in that Host terminal. On Linux/WSL, a
background Host can be targeted by its authenticated private socket peer (the
following commands are for the user to execute; do not delete sockets, locks or
Session files):

```bash
set -e
cd /home/e230038/src/unreal-agent
python3 - <<'PY'
import os, signal, socket, struct, time
from pathlib import Path
path = str(Path.home() / '.local/state/unreal-agent/claude-tool-bridge-smoke/run/host.sock')
s = socket.socket(socket.AF_UNIX)
try:
    s.connect(path)
except (FileNotFoundError, ConnectionRefusedError):
    s.close()
    print('No running Host at the smoke socket; launcher will recover it.')
    raise SystemExit(0)
pid, uid, _ = struct.unpack('3i', s.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, 12))
s.close()
argv = Path(f'/proc/{pid}/cmdline').read_bytes().split(b'\0')
exe = os.readlink(f'/proc/{pid}/exe').removesuffix(' (deleted)')
expected = str(Path('/home/e230038/src/unreal-agent/bin/unreal-agent').resolve())
if uid != os.getuid() or exe != expected or b'serve' not in argv or path.encode() not in argv:
    raise SystemExit('Socket owner did not match the expected smoke Host; not signaled.')
os.kill(pid, signal.SIGTERM)
deadline = time.monotonic() + 15
while Path(f'/proc/{pid}').exists():
    if time.monotonic() >= deadline:
        raise SystemExit('Old Host is still exiting; wait for it before restarting.')
    time.sleep(0.1)
PY

bin/unreal --config "$HOME/.config/unreal-agent/claude-tool-bridge-smoke.json" \
  --state-directory "$HOME/.local/state/unreal-agent/claude-tool-bridge-smoke" \
  --socket "$HOME/.local/state/unreal-agent/claude-tool-bridge-smoke/run/host.sock" \
  claude-tool-bridge-smoke
```

Resume uses the original unacknowledged input and retains the failed Turn.
`/retry` is for an uncertain input acknowledgement, not a provider failure.
Do not paste the same task a second time while that input is being resumed.
When a new input is appropriate, send:

```text
このworkspaceをUnreal Agentのtoolだけを使って調査してください。

1. ファイル一覧を確認
2. AGENTS.mdを読む
3. answer.goを読む
4. Answer を検索
5. go test ./... を実行

まだファイルは修正しないでください。
観測した結果だけ報告してください。
```

Check `/view orchestration`: read/search/command work must appear as Unreal
Operations. The deliberately failing test must return its canonical receipt to
Claude. Native Claude tools and foreign MCP must remain absent. If organization
policy still refuses a call, expect a safe typed denial rather than
`system_unknown_shape`; there is no fallback. This manual request is not run by
automated validation.

## MCP compatibility: config, permission and limits

```json
{
  "Runtime": {
    "ClaudeCode": {
      "managedPolicyMode": "trust",
      "ToolBridge": {
        "Enabled": true,
        "MaxToolRounds": 32,
        "TimeoutMillis": 300000,
        "ResultBytes": 65536
      }
    }
  }
}
```

`Enabled` defaults to false. Zero limits use the defaults above. The maximum
accepted bounds are 128 tool callbacks, 3,600,000 ms per callback, and 1 MiB per
result. Invalid limits produce a typed error. Calls execute sequentially through
the host rendezvous; this limit counts callbacks, including failed calls.

`Permissions.Tools` and rooted filesystem/network checks still authorize work;
bridge enablement grants no tool capability or unrestricted access. Invalid
arguments, stale mutations, permission denials, process exits and canceled
Operations return failure receipts, never shell fallback. A nonzero command exit
is an MCP `isError` even though the existing Shell Operation correctly records
the process as completed with its exit code.
Native edit `no_match` likewise returns an error receipt through the shared
translator's typed outcome classifier; its existing completed Operation status
and the original canonical payload remain intact.

**Current permission limitations:** this repository implements concrete allow /
deny capabilities and a typed unsupported-process-sandbox failure. It has no
human `ask` approval workflow or semantic Git approval engine. The bridge does
not invent or auto-approve one. `ProcessMode: unrestricted` explicitly permits
ambient process/filesystem/network access and cannot be combined with restricted
filesystem/network roots. It is not an OS sandbox. Native file mutations remain
rooted to their workspace. Do not grant command capability if ambient command
access is unacceptable. The existing default-deny policy still applies.

For Team/Enterprise, `managedPolicyMode: trust` is a separate explicit
administrative boundary. It means the user trusts organization policy; it does
**not** mean Unreal certified policy safety. Managed hooks, env, telemetry,
helpers and runtime refresh may have effects outside Unreal Operations. Parent
API/GitHub/OpenAI credentials, routing overrides and OTel credentials are not
inherited by Claude. Claude alone reads its login. Unreal does not import tokens
or parse Claude credential/managed-policy contents.

## Canonical lifecycle and context

Each public tool request is committed before translation or dispatch. Each tool
result comes from the normal canonical terminal receipt. The Coordinator appends
a regular continuation Turn with the original runtime revision and input
watermark, maintaining the one-response-per-Turn store contract. Newly queued
user/peer inputs wait for the final response and are not acknowledged by a tool
result. Provider/model/effort changes apply at the next completed boundary.

SDK control IDs, Claude private reasoning, signatures, CLI session IDs and raw
continuation objects remain request-local. MCP identities cache completed
receipts to prevent duplicate execution within a request, including a new control
envelope with the same RPC id. A conflicting reused identity fails closed. Two
identical concurrent calls without an unambiguous mapping fail closed rather than
guessing. Canonical Operations retain their existing idempotency and recovery.

A restart/resume reconstructs public context and Operations through Unreal; it
never uses Claude `--resume` / `--continue`. A lost private continuation is not
restored. Completed edits/commands are not replayed by the bridge; unfinished
Operations follow existing recovery semantics, including unknown outcomes that
must not be blindly executed again. Public context uses the shared Context
Engine's bounded package and inherited Project Instructions snapshot. There is
no second history, full-history replay, generative summary or filesystem reread
of project instructions in the adapter.

During a live request the adapter conservatively accounts for public tool calls
and results against the remaining input budget. Head/tail bounds include explicit
truncation metadata and retain the full canonical receipt in Unreal. Protected
credential-shaped receipts are withheld. Already materialized ViewImage receipts
can map to bounded MCP image content; the adapter never fetches an image itself.
On exhausted context/round/time budgets the request fails with a typed error and
keeps completed canonical work. There is no mid-process compaction or automatic
provider fallback. An explicit later turn/retry rebuilds public context.

## Children, Orchestration, analysis

`SubagentStart` uses the existing provider-neutral runtime request. Unreal
validates provider health, catalog model/effort, template, depth/limits and
permissions before creating the child. Omitted selection is copied at creation;
later parent changes do not change it. Child permissions stay bounded by the
parent. The inherited Project Instructions snapshot is unchanged. A Claude
bridge child uses the shared `Finish` Operation; a disabled/text-only child keeps
its existing durable text completion path. Provider transport approval does not
grant a child command permission.

`/view orchestration` and Wide `/view split` display the same Operations/children
as Codex. No special bridge UI or canonical display mode is added. `/analyze`
Overview shows bridge capability and Claude built-ins blocked; Tools/Usage/Turns
derive from the same history. Aggregate CLI usage is reduced by already recorded
per-response usage, preventing double counting. Unknown fields remain unknown.
Analysis exports contain only safe metadata. Public conversation `/export` keeps
its own canonical source and security exclusions.

## Manual smoke (user executes; no AIdea run)

First verify login without inference:

```sh
command -v claude
claude --version
claude auth status
```

Create a fresh private workspace/config from the existing Team smoke config.
This never overwrites that config or changes any saved Session:

```sh
cd /home/e230038/src/unreal-agent
python3 - <<'PY'
import json, os, pathlib, tempfile
home = pathlib.Path.home()
source = home / '.config/unreal-agent/claude-team-smoke.json'
config = json.loads(source.read_text())
workspace = pathlib.Path(tempfile.mkdtemp(prefix='unreal-claude-bridge-smoke-'))
(workspace / 'go.mod').write_text('module bridge_smoke\n\ngo 1.24\n')
(workspace / 'answer.go').write_text('package bridge_smoke\nfunc Answer() int { return 0 }\n')
(workspace / 'answer_test.go').write_text('package bridge_smoke\nimport "testing"\nfunc TestAnswer(t *testing.T) { if Answer() != 42 { t.Fatal("expected 42") } }\n')
(workspace / 'AGENTS.md').write_text('Work only in this smoke workspace. Do not commit, push, or access credentials.\n')
runtime = config['Runtime']
runtime['Workspace'] = str(workspace)
runtime['SystemPrompt'] = 'Use only the provided Unreal tools. Inspect before editing. Report observed results.'
runtime['DisallowedTools'] = []
runtime['ClaudeCode']['managedPolicyMode'] = 'trust'
runtime['ClaudeCode']['ToolBridge'] = {'Enabled': True, 'MaxToolRounds': 32}
config['Permissions'] = {'Tools': ['read','grep','glob','write','edit','Bash'],
  'ProcessMode': 'unrestricted', 'FilesystemUnrestricted': True, 'NetworkUnrestricted': True}
config['Subagents'] = {}
path = home / '.config/unreal-agent/claude-tool-bridge-smoke.json'
with path.open('x') as f:
    os.chmod(path, 0o600)
    json.dump(config, f, indent=2)
    f.write('\n')
print('Workspace:', workspace)
print('Config:', path)
PY
bin/unreal --config "$HOME/.config/unreal-agent/claude-tool-bridge-smoke.json" \
  --state-directory "$HOME/.local/state/unreal-agent/claude-tool-bridge-smoke" \
  --socket "$HOME/.local/state/unreal-agent/claude-tool-bridge-smoke/run/host.sock" \
  claude-tool-bridge-smoke
```

The command policy above explicitly grants ambient commands; the workspace is
temporary, not a process sandbox. On repeat runs reuse the created config and
launcher command. Use `/model` to select Claude → a discovered model → effort.
Then send these tasks individually:

1. Read `answer.go` using the Unreal read tool and search for `Answer`.
2. Create `notes.md`, reread its revision, and make a small edit.
3. Run `go test ./...`, report the failure, fix `Answer` to return 42 through
   Unreal edit, and rerun the tests.
4. Initialize Git **without a commit**, use `git add -N answer.go`, and inspect
   `git status --short` / `git diff`. Do not commit or push.
5. Inspect `/view orchestration`, `/analyze Tools`, `/analyze Usage`,
   `/analyze Context`, `/export last`, then detach and reconnect with the same
   launcher command. Resume preserves canonical work and runtime selection.

For child smoke, make another new configuration/Session boundary. The following
uses the workspace above and adds the existing external Codex profile plus a
bounded `worker`; it never changes either previous configuration or history:

```sh
python3 - <<'PY'
import copy, json, os, pathlib
home = pathlib.Path.home()
config = json.loads((home / '.config/unreal-agent/claude-tool-bridge-smoke.json').read_text())
config.setdefault('Providers', {}).setdefault('openai-codex', {
  'Provider': {'version': 1, 'provider': 'openai-codex',
    'model': {'id': 'gpt-6.1-sol', 'family': 'openai-reasoning', 'capabilities': ['tools','reasoning']},
    'endpoint': 'https://chatgpt.com/backend-api/codex',
    'auth': {'provider': 'openai-codex','method': 'oauth','id': 'external-codex'},
    'maxAttempts': 1, 'source': 'operator configuration'},
  'ReasoningEffort': 'medium'})
tools = config['Permissions']['Tools']
tools.extend(name for name in ['SubagentStart','SubagentSend','SubagentCancel','SendParent','Finish'] if name not in tools)
child = copy.deepcopy(config['Runtime'])
child['SystemPrompt'] = 'Complete the delegated task using only the provided Unreal tools. Call Finish alone when done.'
workspace = child['Workspace']
config['Subagents'] = {'worker': {'Runtime': child, 'Permissions': {
  'Tools': ['read','grep','glob','SendParent','Finish'],
  'ReadRoots': [workspace], 'NetworkOrigins': ['https://chatgpt.com']}}}
path = home / '.config/unreal-agent/claude-tool-bridge-children.json'
with path.open('x') as f:
    os.chmod(path, 0o600)
    json.dump(config, f, indent=2)
    f.write('\n')
print('Config:', path)
PY
bin/unreal --config "$HOME/.config/unreal-agent/claude-tool-bridge-children.json" \
  --state-directory "$HOME/.local/state/unreal-agent/claude-tool-bridge-children" \
  --socket "$HOME/.local/state/unreal-agent/claude-tool-bridge-children/run/host.sock" \
  claude-tool-bridge-children
```

Use `/model` to confirm the registered Codex model/effort and discovered Claude
model/effort before requesting them. `gpt-6.1-sol` above is an explicit smoke
selection, not a fallback catalog. If it is not in the actual catalog, explicitly
choose one that is; do not probe by inference.

Ask Claude to call `unreal_SubagentStart` with `template: worker`, task “Read
answer.go, report the current Answer value, then call Finish alone with status
completed, that summary, and empty changedFiles/tests/blockers”, and runtime
`openai-codex / gpt-6.1-sol / medium`. After its canonical result, request the same
read-only task using a discovered `claude-code` model/effort. Confirm both children
and their completion in `/view orchestration` / `/view split`. Child commands,
permissions and provider credentials remain Unreal-owned. Do not add `Bash` to
a bounded child expecting an OS sandbox: its process-denied policy rejects it.
The Claude → Codex and Claude → Claude tool requests, duplicate start protection,
and pre-existing reverse-provider API path are covered by offline fixtures.

## Development task execution

Development source changes use the existing native `write`/`edit` Operations.
Their mutation service remains workspace-bound even when an explicit command
profile grants ambient filesystem access. Use owned command Operations for
temporary test harnesses outside the checkout; an out-of-workspace native write
fails rather than expanding that boundary.

The `Bash` registry name does not override the configured command shell. With
the default `/bin/sh`, use POSIX-compatible test wrappers and preserve the test's
exit status after printing captured output. Bash-specific constructs such as
`PIPESTATUS` require explicitly invoking Bash. A passing pytest summary followed
by a wrapper error is still a failed command receipt.

Keep large implementation requests within the configured generation timeout by
splitting them into focused tasks and native edits. A timed-out generation emits
no proposal or new Operation. Resume reconstructs work from public canonical
calls and receipts; an already completed action is not an instruction to execute
its side effect again. Command permissions, context budget and action limits
remain explicit operator configuration, independent of bridge enablement.

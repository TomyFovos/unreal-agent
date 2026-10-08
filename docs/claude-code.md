# Claude Code subscription backend

`claude-code` is an installed CLI provider with **text-only** capability by default.
Explicit opt-in enables [Unreal-owned structured actions or SDK MCP](claude-tool-bridge.md).
New normal bootstrap explicitly selects structured mode; legacy enabled configs
without a mode remain MCP. Claude side-effecting built-ins remain disabled; only
structured mode permits the read-only `StructuredOutput` output serializer.
For daily `unreal` / `unreal my-project` startup, additional provider discovery
and policy-blocked text capability, see [normal launcher](normal-launcher.md).
It is separate from a future `anthropic` API provider. The existing Codex,
OpenAI API, Ollama, OpenRouter and Fireworks providers remain available.

The contract was checked against Claude Code **2.1.285**. Startup and every
request check version, required flags and `claude auth status`, without a
speculative inference. Only the CLI reads and manages its authentication. Unreal
does not read Claude credential files, import OAuth, log in, refresh or log out.
Unsigned authentication remains a typed `external_reauth_required` failure and
instructs the user to run `claude auth login`.

## Text-only model requests

Runtime composition uses the provider descriptor's `tools` capability before
building model requests. A text-only adapter gets an empty execution registry,
no tool schemas in `llm.Request.Tools`, and a text-only harness preamble. That
preamble does not request tool calls or describe asynchronous tool execution;
the Host's existing lifecycle guidance and persisted project instructions still
apply. Existing tool-capable providers keep their catalogs and schemas.

`Permissions.Tools` is an execution allowlist, not the provider's model tool
catalog. An empty allowlist denies tool execution but has historically not
hidden tool definitions for tool-capable providers. Text-only request selection
is independent of that permission policy; no permission grant is widened.

Normal-launcher Claude registration additionally narrows bridge schema exposure
to the configured tool allowlist so exact managed grants cover only permitted
Unreal tools. Codex's existing schema catalog is unchanged. Live bridge readiness
can select text-only requests without rewriting the configured bridge intent or
canonical runtime identity; `/model` and `/analyze Overview` show that status.

In disabled/text-only mode, the Claude adapter still rejects manually supplied
tool schemas before starting the CLI. It also rejects tool-shaped output and stream initialization exposing
available tools or MCP servers. Agent/skill/plugin catalogs in init are metadata,
not evidence that their executable components are available or ran.
Actual agent/task execution is rejected. The `tools_unsupported` type is unchanged;
its safe message distinguishes `request included tool schemas`, `Claude stream
initialization violated isolation`, and `Claude stream contained non-text tool
output`. These diagnostics contain no raw stream, tool arguments or names.
An error with this code alone does not prove that the request included tools.

日本語: text-only provider は runtime の capability 選択で `Tools=[]` と空の
実行 registry を構成します。共通 prompt から tool 利用指示も除きます。
provider 側の非空 tool schema 拒否と tool-shaped stream 拒否は維持します。
`Permissions.Tools=[]` は実行権限の拒否であり、tool 対応 provider の既存 catalog
を変更する指定ではありません。拒否箇所は secret を含まない固定文言で区別します。

### Stream initialization contract (2.1.285)

The isolated CLI reports `permissionMode: "default"` alongside `tools: []`
and `mcp_servers: []`. The adapter requires these fields and a valid model ID.
`default` names the standard authorization mode; it does not enable tools.
`--permission-prompts none` separately controls who answers permission prompts
in print mode. It is compatible with that default mode and remains mandatory.
Other permission modes, including `bypassPermissions`, are unsupported and fail
closed in both init and any status update. See the
[CLI flag reference](https://code.claude.com/docs/en/cli-reference).

The adapter discards agent/skill/plugin catalog metadata and accepts
only the documented status values (`requesting`, `compacting`, or null) after
initialization. Status, rate-limit metadata and private reasoning do not become
conversation text or canonical records. Missing/null tools or MCP arrays,
malformed records, duplicate init, nonempty tools/MCP and actual
tool output still fail closed. Nested tool blocks, tool-input deltas, MCP calls,
subagent output, task events and tool progress/summary records are also rejected.
The wire shapes are covered by synthetic fixtures based on the official
[Agent SDK 0.3.285 types](https://www.npmjs.com/package/@anthropic-ai/claude-agent-sdk/v/0.3.285).

The public `SDKSystemMessage.plugins` shape is an array of objects with `name`
(string), `path` (string), and optional `version` (string). It contains no explicit
executable/enabled component field. The SDK's `plugin_errors` documentation
discusses plugin loading, but neither a row nor the absence of errors proves that
executable components are enabled for a safe-mode invocation. No plugin names,
paths, versions, configuration or error bodies enter Unreal output or history.

The official [safe-mode contract](https://code.claude.com/docs/en/cli-reference)
disables plugins, skills, custom agents and MCP, including managed plugins,
managed skills and policy-configured MCP, while keeping authentication normal.
Managed policy and its documented hook/command exceptions remain subject to the
explicit administrative trust boundary below. Before auth/model execution the
adapter checks the actual argv for safe-mode, restricted mode, disabled tools,
strict empty MCP, disabled customization sources and the other isolation flags.
Missing, duplicated or changed flags fail without starting that subprocess.

Observed facts: the reported real 2.1.285 safe-mode init had two plugin rows,
empty tools/MCP and default permission mode; the public plugin type has only
identity/version fields. The real row field types have not been captured here.
Interpreting those rows as catalog metadata is consistent with the official
safe-mode contract. Whether the CLI enumerates them before disabling components
is an implementation hypothesis, not an observed fact. Array length alone is
therefore not used to decide execution capability. Actual tool/MCP/agent events
always fail, regardless of catalog metadata or managed policy mode.

日本語: `permissionMode=default` は標準の権限判断モードで、tool が有効という
意味ではありません。`--permission-prompts none` は prompt の応答経路を制御する
別の設定です。init の tools/MCP は明示的な空配列を要求し、agents/skills/plugins の
catalog 列挙は実行と区別します。status は本文や history に保存しません。
実際の tool/MCP/subagent 実行イベント、危険・未対応の permission
mode、壊れた init は引き続き拒否します。
起動前に必須 flag を検証し、safe-mode・空 tools・strict な空 MCP 等が欠ければ
subprocess を起動しません。公開 schema の plugin 要素は名前・path・任意の version
のみで、実行可能状態の field はありません。実機で metadata 2 件が列挙されたことは
確認済みですが、列挙タイミングや各実機要素の構造は未確認です。
`CLAUDE_CODE_SIMPLE` と `--bare` は subscription OAuth を読まないため使用も継承も
しません。managed policy の hook 等の例外は `managedPolicyMode=trust` の境界です。

### Closed stream diagnostics

Initialization failures now carry a fixed reason code, before the existing
descriptive diagnostic so it remains visible in a bounded terminal display:

```text
init_mcp_nonempty(count=1): claude-code is text-only: tools unsupported (Claude stream initialization violated isolation)
```

| Reason | Existing rejection condition |
| --- | --- |
| `init_duplicate` | A second init before the final result |
| `init_tools_nonempty` | Init advertises available tools |
| `init_mcp_nonempty` | Init advertises MCP servers |
| `init_plugins_loaded` | Reserved; catalog presence never emits this reason |
| `init_permission_mode` | Unsupported permission mode; an empty mode is malformed |
| `init_missing_required_field` | Missing model, tools, MCP or permission mode |
| `init_null_required_field` | Null model, tools, MCP or permission mode |
| `init_wrong_field_type` | Init fields cannot decode into the existing wire types |
| `init_agents_invalid` | Agent catalog has an invalid type/element |
| `init_skills_invalid` | Skill catalog has an invalid type/element |
| `init_model_invalid` | Model fails the existing public identifier validation |
| `init_unknown_shape` | Unreadable/malformed initialization or missing initialization |

Status errors are distinguished with `status_before_init`,
`status_missing_required_field`, `status_wrong_field_type`,
`status_unknown_value`, `status_permission_mode`, `status_tools_nonempty`,
`status_mcp_nonempty`. `status_plugins_loaded` is also reserved and is not emitted
for catalog metadata. An unsupported system subtype or malformed supported
metadata event uses `system_unknown_shape`; actual tool/subagent output uses
`stream_tool_execution`. If several predicates match, the existing validation
order determines the first reason; tools/MCP checks still precede the
duplicate/required-field checks.

Initialization/status errors retain a closed numeric enum and a nonempty array's count.
`Error.StreamReason()` exposes the fixed code to callers; the existing typed
error codes remain unchanged. No raw JSON, decoder error, backend stderr,
field values, catalog names, prompts, session/account identity or private reasoning is
included. Malformed input remains `malformed_stream`; unavailable tools remain
`tools_unsupported`. Plugin metadata no longer causes an isolation rejection;
execution detection and the required launch isolation remain mandatory. No real
backend request is run automatically to collect metadata.

日本語: typed error code と実行拒否の isolation は維持します。plugin catalog の
件数による拒否は行いません。init/status の診断は固定 reason code と該当配列の件数で、
raw JSON、値、catalog 名、credential、prompt、session/account ID、private reasoning は
出しません。catalog の値から実行権限を推測する処理も行いません。

### System metadata and safe shape diagnostics

The public [SDK 0.3.285 schema](https://unpkg.com/@anthropic-ai/claude-agent-sdk@0.3.285/sdk.d.ts)
defines additional system messages. It is a vocabulary reference, not a blanket
permission to accept every event. Only the following additional records are
accepted after an isolated init, with exact field types and their documented
required fields. Unknown fields, missing/null required fields, unsupported enums
and tool/subagent markers fail closed.

| Subtype | Classification and adapter behavior |
| --- | --- |
| `init`, `status` | Existing isolation and status validation |
| `api_retry` | Validated retry metadata; ignored, without duplicating a request |
| `thinking_tokens` | Estimated counters; ignored, never reasoning text or known usage |
| `informational` | General notice; ignored only without a tool link; `prevent_continuation=true` is a safe subprocess failure |
| `notification` | Validated notice; text/key/color ignored |
| `session_state_changed` | CLI-local `idle`/`running`; ignored, never Host lifecycle; `requires_action` is unsupported |
| `task_started`, `task_progress`, `task_updated`, `task_notification` | Subagent/task execution; existing rejection |
| `background_tasks_changed`, `permission_denied`, `elicitation_complete` | Task/tool/MCP interaction; unsupported |
| `hook_started`, `hook_progress`, `hook_response`, `local_command_output`, `files_persisted`, `plugin_install` | Execution/customization/persistence related; unsupported |
| `compact_boundary`, `memory_recall`, `model_refusal_fallback`, `model_refusal_no_fallback` | Context or model/refusal semantics; unsupported, not applied to Unreal history |
| `control_request_progress`, `commands_changed`, `mirror_error`, `worker_shutting_down` | Separate control/customization/lifecycle contracts; unsupported |

The real failing subtype has **not been captured**: no real inference was run
to guess it. An unsupported/malformed system record now includes only a safe
schema projection, also available through `Error.SystemShape()`:

```text
system_unknown_shape: type=system subtype=compact_boundary fields=[compact_metadata:object,session_id:string,subtype:string,type:string,uuid:string] object=true type_present=true subtype_present=true: Claude Code returned an invalid or incomplete stream
```

This is a synthetic example, not the observed failure. Subtype and field names
come from closed public-schema lists. Unknown subtype values become `unknown`;
arbitrary field names become JSON-type counts, since a key itself can contain a
secret. At most 32 known top-level fields are retained, with a count for omitted
fields. String/number/boolean/null/array/object are the only reported JSON types.
Nested keys and every field value are discarded. No email, account/org/session
identity, transcript, credential, private reasoning, raw stderr or JSON enters
this diagnostic. The existing SafeText and bounded TUI display still apply.

Catalog and inference have independent `exec.Cmd`, temporary cwd, process group,
stdin/stdout pipes and parsers. Catalog sends `initialize`/`list_models` and
returns only cloned model metadata; inference sends context as text stdin and
uses `parseStream`. Every catalog process is canceled/joined and its pipes/cwd
closed before discovery returns. Refreshing during an active request does not
feed control frames to that request. An inference control frame is still an
error. Host `stop_complete` is an Unreal history record for completed control
inputs, never a Claude stream record. Full old-Host shutdown, restart/resume,
refresh, model switch and subsequent fake inference are covered by offline tests.

日本語: 今回実機で失敗した subtype は未観測です。推測で全 system event を許可せず、
上記 metadata の shape を個別に検証して破棄します。通知本文・推定カウンタは history、
progress、usage、Host lifecycle に使いません。未知 event は固定 reason と公開 schema
名・JSON type・presence のみ表示します。未確認の名前も値もそのまま出しません。
catalog process と inference process は独立し、refresh の control stream を通常応答に
混ぜません。built-in tools、MCP、subagent/task の実行拒否と text-only contract は維持します。

## Managed policy modes

`Runtime.ClaudeCode.managedPolicyMode` accepts `reject` or `trust`. Omission
means **reject**; every other value is a configuration error. Existing configs
keep their creation identity when the field is omitted. For example, add this
field to the existing `ClaudeCode` executable/catalog configuration:

```json
{
  "Runtime": {
    "ClaudeCode": {
      "managedPolicyMode": "trust"
    }
  }
}
```

This is a configuration fragment, not a replacement for the model, permissions
or provider configuration shown below. The trust choice is part of the immutable
Session creation identity. Resume keeps it; changing the mode requires a
separate Session and an appropriately configured Host. `/model` still changes
only model/effort through the existing canonical selection revisions.

**`managedPolicyMode=trust` means the user explicitly trusts organization-managed
Claude Code policy as an external administrative boundary. It does not mean
Unreal Agent verified that policy to be safe.** Team and Enterprise first-party
subscription logins can use this mode. No policy payload or doctor result is
read to authorize it. A remote cache, a possible runtime fetch, endpoint policy,
WSL inherited policy or unknown managed-policy state does not negate that
explicit opt-in. Authentication must still report a supported subscription,
`claude.ai` authentication and the `firstParty` provider.

The text-only invocation below keeps Claude built-in tools disabled, an empty MCP
configuration, disabled user/project settings, and no Claude session persistence.
Unreal Session, context and Operations remain canonical. Text-only mode rejects
a stream exposing available tools, MCP or
actual agent/tool execution is rejected; init agent/skill/plugin catalog metadata
does not enable execution.

Organization-managed hooks, environment settings, policy helpers, telemetry
and server-managed runtime refresh may have side effects
**outside Unreal Agent's Operation boundary**. Trust mode accepts these as
administrator-controlled behavior, including intentionally configured audit
communication. `--safe-mode` and `disableAllHooks` do not prove their absence or
suppress managed hooks. The CLI remains responsible for that administrative
policy, its authentication and telemetry. Unreal does not copy the policy into
canonical state, export its contents, or reconstruct its audit log.

日本語: `managedPolicyMode=trust` は、組織管理者の Claude Code policy を
外部の信頼境界としてユーザーが明示的に信頼する設定です。Unreal Agent が
policy の安全性を検証したという意味ではありません。Claude built-in tools は
引き続き無効です。組織 managed hooks、env、telemetry、policy helper、managed
MCP に関連する管理者動作や runtime refresh の副作用は、Unreal Agent の
Operation 境界外で発生し得ます。既定の `reject` は従来の fail-closed 動作を維持します。

### Reject mode isolation blocker

The installed `--bare` mode and `CLAUDE_CODE_SIMPLE=1` exclude stored subscription
OAuth. Neither is used or inherited. This backend uses `--safe-mode` and `--restricted`,
which preserve normal subscription authentication, plus an empty tool set and
disabled settings sources. See the [CLI reference](https://code.claude.com/docs/en/cli-reference).

Managed policy is an exception: safe mode retains policy hooks, and ordinary
`disableAllHooks` settings cannot disable them. See the [hook hierarchy](https://code.claude.com/docs/en/hooks#disable-or-remove-hooks).
In the default `reject` mode the adapter refuses system managed settings/MCP,
macOS managed preference files and a `remote-settings.json` cache by **metadata only**, without reading
their contents. Existing source metadata means executable policy cannot be
ruled out; it does **not** establish whether the cached policy is active or empty.
Unreadable/error states remain unknown and are rejected. Remote-fetch eligibility
is checked separately: Team/Enterprise stored logins can receive policy during a
model request, while an unknown credential classification cannot certify its
absence. These return the existing typed `policy_isolation_unavailable` error,
with a closed explanation for source presence, mutable remote policy or unknown
evidence. No plan/account string or raw diagnostic is included in the error.
[Server-managed delivery](https://code.claude.com/docs/en/server-managed-settings)
can install hooks at startup and while a session is active.

The existing Pro/Max allow set is unchanged: under the verified first-party
stored-login contract those subscriptions do not trigger server-managed fetch.
This is a delivery-eligibility distinction, not an entitlement restriction.
Reject mode does not allow Team/Enterprise. Do not remove a policy
cache, change the auth-status result, or bypass administrator policy to make
preflight pass. Trust mode instead requires explicit acceptance of the external
administrative boundary; it makes no managed-policy isolation claim.

The development machine reported a Team subscription and a remote settings
cache. Its Claude smoke test remains **blocked at preflight in reject mode**;
the separate trust-mode smoke config below permits that Team login without
inspecting its organization policy. No hosted request was performed during
development or validation.

### Managed-policy diagnostics investigation (2.1.285)

The [official diagnostics description](https://code.claude.com/docs/en/server-managed-settings#verify-settings-delivery)
documents a `Managed settings (remote)` line in `claude doctor` (2.1.248+).
On the installed 2.1.285 binary, `claude doctor --help` exposes only help; no
doctor-specific JSON output or versioned machine-readable result is available.
Read-only inspection of the installed command registration and formatter
confirmed the following human-readable states. These are observations for this
version, not a published stable parsing/schema contract:

| Diagnostic outcome | What it establishes | Authorization of a fresh `-p` request in reject mode |
|---|---|---|
| Organization has no configured remote settings | The completed fetch in that doctor process returned no settings | No: a fresh process fetches again |
| Settings loaded | That fetch delivered policy | Rejected |
| Fetch failed, no policy applied | The doctor process could not establish current server state | Rejected as unknown |
| Fetch failed, stale cache applies | Cached policy remains in effect | Rejected |
| Fetch skipped, with a reason | The doctor process did not perform a fetch | Rejected as absence evidence; routing, auth or entrypoint may differ |
| Fetch in progress | No completed result yet | Rejected as unknown |
| Missing, changed or ambiguous output | No recognized result | Rejected as unknown |

The human-readable no-configuration phrase is `none configured for this
organization` in this installed version. It is not an attestation covering a
subsequent process. Exit code 0 alone does not identify the remote settings state.
The adapter does not invoke doctor or accept its output as a policy-bypass
option. The actual doctor action was **not** executed during this investigation:
only its parser help was run. Its settings initialization can load endpoint
policy/helpers; no helper or hook was fired to test whether it was harmless.

The [official fetch/caching contract](https://code.claude.com/docs/en/server-managed-settings#fetch-and-caching-behavior)
fetches at startup and refreshes hourly; cached policy can apply before the
fetch completes. Non-interactive `-p` runs also apply newly delivered settings
that would require an approval dialog in an interactive session. Therefore even
a trustworthy doctor "none" snapshot leaves a preflight-to-request race.
`forceRemoteSettingsRefresh` waits for a successful fresh fetch and **applies**
its policy; it neither pins absence nor prohibits executable policy. No official
request-bound no-policy lease, immutable policy snapshot or pre-execution veto
was found in the verified interface. Stream init/tool checks happen too late to
prevent a startup helper or `SessionStart` hook.

Endpoint policy must be considered independently of remote settings. The
[endpoint-managed contract](https://code.claude.com/docs/en/managed-settings)
includes `managed-settings.json`, `managed-settings.d`, `managed-mcp.json`,
macOS managed preferences and Windows HKLM/HKCU registry policy. A `policyHelper`
can itself run an executable before producing settings. The adapter's native
file/cache metadata checks are conservative source checks, **not** a complete
OS/MDM attestation.

In particular, `/etc/claude-code` being absent on WSL does not prove Windows
policy absent. `wslInheritsWindowsSettings` can enable inherited HKLM, Windows
managed files and opted-in HKCU policy. This machine is WSL2; no Windows
executable/registry probe was run and no WSL inherited-policy absence was
certified. macOS managed-preference file absence alone is likewise not a general
MDM-domain attestation. These gaps cannot justify Team support in reject mode.

A Team allowance under a complete-isolation contract would require both
endpoint-policy absence and an official guarantee that no server/endpoint policy capable of commands, hooks,
helpers or other side effects can arrive or execute for the whole actual
request. A plan name, an empty cache, a successful doctor snapshot, safe mode,
tool disabling and repeated preflight checks do not supply that guarantee.
Until the missing contract is available, Team/Enterprise requests in reject mode
remain fail-closed; credential files, settings and administrator policy stay untouched.

Trust mode deliberately does not claim that complete isolation contract and
does not use these diagnostics to authorize managed policy.

## Tools and Session ownership

The strict empty-MCP and tool-output rejection descriptions earlier in this
document apply to the disabled/text-only and catalog-discovery transports.
Structured Action mode retains empty MCP and permits only the side-effect-free
`StructuredOutput` serializer. MCP compatibility mode replaces the empty MCP
boundary with an attested caller-owned SDK server. Side-effecting built-ins,
foreign MCP and Claude task execution remain unavailable in both modes.

The official [custom tool integration](https://code.claude.com/docs/en/agent-sdk/custom-tools)
uses MCP handlers in the Claude agent loop. The [loop contract](https://code.claude.com/docs/en/agent-sdk/agent-loop)
executes and feeds tool results internally. The explicit MCP mode of
[Tool Bridge v1](claude-tool-bridge.md) uses the official SDK MCP control transport
and a provider-neutral Coordinator rendezvous during `llm.Adapter.Respond`.
SDK handlers forward requests to Unreal and return canonical receipts; they do
not run tools. The continuation stays private to that one request's process.

Structured mode uses official `initialize.jsonSchema` and
`result.structured_output` instead. Unreal independently validates each Action,
executes it through the same Coordinator rendezvous, and rebuilds bounded context
from canonical receipts for a new generation. It needs no MCP grant or private
continuation. New normal-launcher bootstrap explicitly selects this mode;
existing bridge configurations without a mode retain MCP compatibility.

In disabled mode it declares no `tools` or `images` capabilities. The Runtime owns an empty
Translator registry, passes no tool definitions, and rejects tool-shaped stream
records. CLI built-ins and MCP are disabled; no Claude callbacks execute tools.
Claude never receives Subagent tool schemas. In a multi-provider Host, operator
commands/core APIs can start a registered Codex or Claude child independently of
the parent provider. A Claude child completes from its durable text response via
an Unreal-owned canonical Finish record; it creates no Finish Tool Operation.
In opt-in bridge mode, the existing Subagent Operations can also be requested by
Claude. A tool-enabled Claude child finishes with the shared Finish Operation,
while keeping its bounded permissions and its own runtime. The existing Codex Finish tool path is unchanged. See [multi-provider runtime
and child configuration](multi-provider.md). Legacy single-provider fixed-template
configurations keep their original subagent compatibility checks.

Every model request starts a fresh process. `--continue` and `--resume` are never
used. Unreal history, `runtime_selection`, `runtime_applied` and
`Turn.RuntimeRevision` remain canonical. CLI session persistence, auto memory,
auto compaction, built-in subagents, plugins, skills, user/project hooks, non-Unreal MCP, IDE integration
and prompt suggestions are disabled by the invocation/configuration. Managed
administrative behavior is the external boundary described above. Model/effort
confirmation is applied by the existing next-turn boundary and restored on Host restart/resume.

The Context Builder's system/project instructions go to a temporary 0600 file
inside a 0700 directory. Current user text goes to stdin. On later turns the
builder's prior user/assistant messages are serialized as Role/Text context in
one stdin input. This preserves their contents but is not native role-history
replay in Claude's CLI. The multi-provider Context Builder projects historical
tool receipts as quoted data and omits foreign private reasoning/state; it does
not give Claude access to the tools that produced them.
Temporary files are removed after the process and pipes drain.

## Invocation and permissions

The adapter uses Go `exec.CommandContext` directly, without a shell. The model
text-only invocation contract is (the structured and MCP variations are documented
in [Tool Bridge v1](claude-tool-bridge.md)):

```text
claude --safe-mode --restricted --setting-sources ""
  --settings '{"disableAllHooks":true,"autoMemoryEnabled":false,"disableClaudeAiConnectors":true,"syncClaudeAiSkills":false,"syncClaudeAiPlugins":false}'
  --tools "" --disallowedTools "*"
  --strict-mcp-config --mcp-config '{"mcpServers":{}}'
  --disable-slash-commands --no-chrome
  -p --input-format text --output-format stream-json --verbose
  --include-partial-messages --no-session-persistence
  --permission-prompts none --max-turns 1
  --system-prompt-file PRIVATE_TEMP_PATH
  --model CONFIGURED_MODEL --effort CONFIGURED_EFFORT
```

`--system-prompt-file` and `--max-turns` are not listed in every installed help;
their parser availability is checked with the non-inference auth-status action.
Unknown major/minor versions, versions before 2.1.285, or missing required flags
are rejected. Future 2.1 patches must still pass these checks.

The process gets an explicit environment containing HOME, an optional existing
`CLAUDE_CONFIG_DIR` path, a fixed native PATH/locale and adapter-owned disabling
switches. `ANTHROPIC_API_KEY`, OAuth/API tokens, authorization headers, proxies,
cloud credentials, Node options and debug settings are not copied. An unrelated
parent API key does not change the selected billing source. Active Bedrock,
Vertex, Foundry, Mantle/AWS routing or endpoint/header overrides produce an
explicit subscription-mode conflict. There is no API-key or model fallback.
The [environment reference](https://code.claude.com/docs/en/env-vars) documents
the provider switches and the disabling switches used here.

### Stored-login auth preflight regression (2.1.285)

The adapter must leave `CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST` **unset**, including
when it is present in the parent environment. The official environment reference
defines it for embedding platforms that manage Claude's provider routing. This
adapter delegates subscription authentication to the installed CLI and does not
supply host credentials. Unreal Session and tool ownership remain enforced by
the canonical runtime, empty tool registry and isolation flags above.

Non-inference `auth status` measurements on the installed 2.1.285 binary used
the exact production flags and a private `unreal-claude-probe-*` cwd:

| Environment | loggedIn | authMethod | subscriptionType | Exit |
|---|---|---|---|---|
| Previous exact Unreal environment, marker `1` | false | none | null | 1 |
| Same environment with only the marker removed | true | claude.ai | team | 0 |
| Known-good minimal environment, same flags and temporary cwd | true | claude.ai | team | 0 |
| Minimal environment plus marker `1` | false | none | null | 1 |
| Minimal environment plus marker `0` | true | claude.ai | team | 0 |

Removing each other adapter variable, adding USER or LOGNAME, extending PATH,
or changing to the repository cwd did not fix the old environment. USER/LOGNAME
inheritance and PATH expansion were therefore unnecessary. The existing repo
binary matched a build of the unchanged source byte-for-byte; stale build was
not the cause. Only allowlisted nonsecret auth metadata and exit codes were
recorded. No credential file, account identity or model request was involved.

The [official auth-status exit contract](https://code.claude.com/docs/en/cli-reference)
is 0 when logged in and 1 when not, matching these measurements. The existing
typed handling is retained: signed-out status produces `external_reauth_required`,
malformed status produces a safe subprocess error, and unsupported provider or
subscription metadata fails closed. Fake CLI tests reproduce the host-marker
behavior and capture production auth invocation metadata without recording auth
output. All automated provider requests still use that offline fake executable.

日本語: 原因は Unreal が注入していた `CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST=1`
です。同じ binary・argv・private cwd で、この変数だけを外すと既存 Team login の
認識が復旧しました。Claude CLI が認証を所有し、Unreal が Session と tools を
所有する既存契約を維持します。通常の smoke 再実行に login のやり直しや Session
history の削除は不要です。

### Organization OpenTelemetry

In reject mode the adapter retains its telemetry/nonessential-traffic disabling
variables. In trust mode it omits `DISABLE_TELEMETRY`, `DISABLE_ERROR_REPORTING`
and `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC` so organization-managed audit
communication can work. Parent `CLAUDE_CODE_ENABLE_TELEMETRY`, `OTEL_*` values,
exporter endpoints and credentials are **not inherited** in either mode. Claude
Code may itself apply those variables from trusted organization policy after
startup. The adapter does not read their values, provision an exporter, or
duplicate audit logs. See [Claude Code monitoring](https://code.claude.com/docs/en/monitoring-usage#administrator-configuration)
for the administrator-managed telemetry configuration. No real collector or
backend communication is part of the automated tests.

Parent API/routing overrides remain rejected or stripped in both modes. Trust
does not turn a parent API key into subscription authentication or authorize a
local billing/endpoint override. Organization-managed values applied internally
by Claude are a distinct source within the explicit administrative trust boundary.

The existing `Policy.CheckProcess` is enforced for the CLI, including preflight.
The existing policy has no restricted OS process sandbox: it requires an
explicit unrestricted process/filesystem/network grant. Tools remain denied by
an empty tool registry and empty permission Tools list. The CLI's transport
cannot be mediated by Unreal's HTTP origin/redirect transport; the existing HTTP
providers retain those protections. Do not assume an HTTP origin allowlist can
contain an arbitrary external executable.

Cancellation first signals the process group, then bounds exit/pipe waits and
kills remaining group members. Every directly owned process is waited/reaped;
request goroutines join before returning. Context cancellation and shutdown
remain distinct from provider failures. The executable provider's runtime Close
also cancels and drains all of its in-flight requests before returning; the
Host/Coordinator lifecycle and existing HTTP providers are unchanged.

## Model picker and analysis

The primary catalog is Claude Code's own selectable-model metadata. On the
installed **2.1.285**, the official SDK stdio transport accepted `initialize`
and `list_models` control requests with no prompt. Both returned 12 model rows
for the development account. That is a local observation, not a shipped model
list or a promise about another organization. The public SDK's
`supportedModels()` returns `initialize.response.models`; that response is used
when `list_models` is unsupported. See the
[published SDK types](https://unpkg.com/@anthropic-ai/claude-agent-sdk@0.3.285/sdk.d.ts).

Discovery uses the same safe-mode/restricted/tools/MCP/environment isolation as
text requests, a private temporary cwd, and no session persistence. Its argv
contains `--input-format stream-json --output-format stream-json --verbose`,
**no `-p`/`--print` and no model prompt**. Stdin contains only the two allowlisted
control requests, with empty SDK hooks/MCP. No user message, tool callback,
filesystem command or inference probe is sent. Unreal never calls `/v1/models`,
reads credentials, or imports OAuth. Raw stderr and initialize account metadata
are discarded; only bounded ModelInfo fields enter the read-only catalog.

Claude evaluates organization `availableModels`, `enforceAvailableModels` and
model overrides. Unreal does not parse managed settings or recompute entitlement.
A successful discovered list is authoritative: configured or previously active
models missing from it are not appended to the picker. A successfully returned
empty list remains empty. See the [model policy contract](https://code.claude.com/docs/en/model-config).

`Runtime.ClaudeCode.Models` is optional and remains an **explicit operator
fallback**. Invalid/duplicate entries still fail validation. On discovery failure
the UI identifies the configured fallback; with no explicit entries it says
catalog unavailable and shows only the canonical current selection/effort.
No static model or effort list is invented. Configured display/default/context
metadata may override a matching returned row, but cannot add a restricted model
or expand its effort levels. Keep the original runtime JSON for an existing
Session: removing/changing its fallback entries still changes creation identity.

ModelInfo `value` becomes picker/selection ID; `resolvedModel` is separate
read-only metadata and can match an existing full-ID selection to an alias row.
Display name/description and optional effort/adaptive-thinking/fast-mode
capability fields are retained. Actual model identity still comes from the
completed stream response and is stored as the canonical model observation.
Alias resolution changes never rewrite earlier turns or selections.

`supportedEffortLevels` is intersected with the harness's valid effort enum;
unknown levels are omitted without translation. Missing/all-unsupported levels
do not get a guessed default. If Claude explicitly reports `supportsEffort=false`,
the second picker confirms **No effort parameter**: the canonical selection
records an empty effort and the CLI receives no `--effort` flag. Other providers
retain their existing requirement for a valid effort. A missing capability flag
is not treated as false.

The Host shares a memory-only one-minute cache with its runtime adapters and
picker. Repeated opens within that lifetime do not start another discovery
process. Executable replacement invalidates the cache; `/model refresh` forces
a fresh process after login/organization-policy changes. Authentication still
runs before each text request. Cache contents, account identity, descriptions,
and resolved-model metadata are not Session state or creation identity.

The existing `/model` infrastructure displays that provider catalog, then the
selected entry's efforts. Both confirmations are required, cancel is local, and
the same canonical revision history controls subsequent turns. No `/effort` or
provider-switching command was added. Header/model presentation and the Live
Dock composer/menu/navigation behavior remain unchanged.

日本語: `/model` は Claude 自身の selectable model catalog を優先します。
organization の allowlist/override は Claude が評価し、Unreal は設定ファイルや
credential を読みません。取得成功時には explicit/current の別モデルを追加しません。
取得不能時だけ既存 `Models` を fallback に使い、未設定なら current selection のみを
表示します。cache は Host 内の 1 分間だけで、`/model refresh` で即時更新できます。
alias と resolved model は分離し、既存 runtime revision と turn の観測 identity を
保持します。effort 非対応が明示された model では、2 段目で「parameter を送らない」
ことを確認し、存在しない effort を生成しません。discovery では `-p`、prompt、user
message、tool command、API inference を送信しません。

Streamed assistant text becomes temporary progress. Completed text, response
ID, observed model identity and normalized usage become the normal model
response. `input_tokens` is combined with cache-read and cache-write counts to
match Unreal's inclusive input convention. Output stays the reported total.
Reasoning counts are unknown; private thinking/signatures are never retained.
Missing usage fields are explicitly recorded as unknown, including zero-valued
fields whose counters were not supplied. The existing viewer/analysis projection
marks incomplete totals partial and does not invent zeros, context percentages,
remaining subscription quota or monetary estimates. Raw costs, account identity
and Claude session IDs are discarded.

All `/analyze` views and JSON/Markdown metadata exports use the existing
projection. They include no prompts, assistant bodies, child bodies, credentials
or private reasoning. Backend errors have a closed safe vocabulary; raw stderr
and diagnostic assistant/error bodies never enter history or output.
Overview now includes provider, model, effort, `managedPolicyMode`, Unreal tool
ownership and blocked Claude tools. Trust mode's meaning is stated in that
read-only sheet. The viewer row and JSON/Markdown exports include the same
nonsecret mode, with no account identity, managed policy contents, collector
endpoint or telemetry credentials. The Live Dock layout is unchanged.

## Manual smoke preparation (no request run automatically)

First inspect the installation and subscription yourself:

```sh
command -v claude
claude --version
claude auth status
```

The following procedure explicitly opts the current Team login into trust mode
with an operator-verified model and effort. It does not replace the default
Codex config or reuse its Session. To use an unmanaged Pro/Max login with the
default policy boundary instead, set `managedPolicyMode` to `reject` in a
separate config.

```sh
cd /home/e230038/src/unreal-agent
export PATH="$PWD/bin:$PATH"
read -r -p 'Verified Claude model ID: ' CLAUDE_TEAM_SMOKE_MODEL
read -r -p 'Verified reasoning effort: ' CLAUDE_TEAM_SMOKE_EFFORT
export CLAUDE_TEAM_SMOKE_MODEL CLAUDE_TEAM_SMOKE_EFFORT
python3 - <<'PY'
import json, os, pathlib, shutil
root = pathlib.Path.home() / '.config/unreal-agent'
root.mkdir(mode=0o700, parents=True, exist_ok=True)
path = root / 'claude-team-smoke.json'
model, effort = os.environ['CLAUDE_TEAM_SMOKE_MODEL'], os.environ['CLAUDE_TEAM_SMOKE_EFFORT']
if not model or not effort:
    raise SystemExit('Explicit verified model and effort are required')
config = {
  'Runtime': {
    'Version': 1,
    'Workspace': '/home/e230038/src/unreal-agent',
    'SystemPrompt': 'Answer the user. This provider is text-only; tools are unavailable.',
    'ReasoningEffort': effort,
    'Profile': {'id': 'conservative', 'version': 1, 'source': 'operator configuration'},
    'Provider': {
      'version': 1, 'provider': 'claude-code',
      'model': {'id': model, 'family': 'claude', 'capabilities': ['reasoning']},
      'endpoint': '',
      'auth': {'provider': 'claude-code', 'method': 'oauth', 'id': 'external-claude-code'},
      'max_attempts': 1, 'source': 'operator configuration'
    },
    'ClaudeCode': {
      'managedPolicyMode': 'trust',
      'Binary': shutil.which('claude'),
      'Models': [{'ID': model, 'Name': model, 'Efforts': [effort], 'DefaultEffort': effort}]
    }
  },
  'Permissions': {
    'Tools': [], 'ProcessMode': 'unrestricted',
    'FilesystemUnrestricted': True, 'NetworkUnrestricted': True
  }
}
if not config['Runtime']['ClaudeCode']['Binary']:
    raise SystemExit('Installed Claude executable not found')
fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
with os.fdopen(fd, 'w') as f:
    json.dump(config, f, indent=2)
    f.write('\n')
print(path)
PY
```

This explicitly grants the existing process policy to the trusted installed CLI;
it is not a change to Unreal's defaults. The exclusive create prevents replacing
an existing config. The model and effort values come from your input; the script
does not discover, infer or populate alternative models.

Use a separate private state/socket so a running Codex Host is not reused:

```sh
unreal --config "$HOME/.config/unreal-agent/claude-team-smoke.json" \
  --state-directory "$HOME/.local/state/unreal-agent-claude-team-smoke" \
  --socket "$HOME/.local/state/unreal-agent-claude-team-smoke/run/host.sock" \
  claude-team-smoke
```

To check only discovery, open `/model` without sending a user prompt. Confirm
`discovered catalog`, navigate through the returned models, and continue to that
entry's effort picker. Cancel preserves the current selection; confirmation
records the existing canonical selection revision without starting a turn.
`/model refresh` updates account/policy metadata immediately. Keep the same
runtime JSON, including its existing fallback entries, for reconnect/resume.

Only for a user-initiated inference smoke after successful preflight, send from the TUI:

```text
Reply with exactly: Claude Team Unreal smoke OK
```

Check the streamed reply, displayed model/effort, `/model` model-to-effort picker
and `/analyze` Overview (managed policy mode: trust) / Usage / Turns / Export.
Detach with Ctrl-D or `/detach`, then rerun the same launcher command to reconnect
to the canonical conversation. For a stopped Session, use `/stop idle`, detach
after it stops, and rerun the same command to verify automatic resume and retained
model/effort/trust mode. No README/tool smoke is
provided: tools are unsupported. No real backend request belongs in CI.

### Reusing a failed text-only smoke Session

Rebuilding the executable does not replace an already running background Host.
After detaching, identify the `serve` process for the exact smoke socket and
session directory, send that process SIGTERM, and wait for its gateway ownership
lock to be released. Do not delete the socket lock, state or Session history.
The launcher can then start the rebuilt Host using the same config/state/socket.

For an input already committed before a model failure, `/retry` is not the
recovery command: it only resends an uncertain submit with the same input ID.
Resume replays the existing canonical input and appends a new ordinary turn;
the failed turn remains in history. Running the same launcher for this stopped
Session resumes it automatically and can immediately issue that pending model
request. There is no need to retype the prompt or create a new Session. From an
attached stopped Session on the updated Host, `/resume` has the same behavior.

If the new failure says `Claude stream initialization violated isolation` or
`Claude stream contained non-text tool output`, its source is the returned CLI
stream, not request tool schemas. The updated init validator accepts the normal
2.1.285 default permission mode and agent/skill/plugin catalog metadata described
above. Remaining failures are still fail-closed; do not enable Claude tools or
MCP to get past them.

日本語: 同じ config/state/socket で再起動した Host に同じ Session 名で戻ると、
既存の未応答 input が resume により再試行されます。failed turn と input ID は
保持されます。再起動後の launcher 起動だけで model request が発生し得るため、
ユーザーが手動 smoke として実行してください。`/retry` は未確認 submit の再送用です。

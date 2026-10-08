# Normal launcher

```sh
unreal
unreal my-project
```

In a source checkout use `bin/unreal` after `make build`. No name selects
`default`. Existing Sessions attach when running and resume when stopped; absent
Sessions are created. `unreal-agent serve`, `attach` and `child` remain available
for explicit low-level use. There is no `unreal init` command.

## Configuration resolution and first run

Resolution is explicit `--config`, then the normal runtime file, then conservative
local bootstrap when that normal file is absent. Explicit missing files and
malformed JSON fail safely; neither triggers bootstrap. Existing files are never
rewritten by the launcher.

| Resource | Default |
| --- | --- |
| Configuration | `~/.config/unreal-agent/runtime.json` |
| State | `~/.local/state/unreal-agent/` |
| Canonical history | `STATE/sessions/` |
| Socket | `$XDG_RUNTIME_DIR/unreal-agent/host.sock` |
| Socket fallback | `STATE/run/host.sock` |
| Host log | `STATE/host.log` |

Existing `XDG_CONFIG_HOME`, `XDG_STATE_HOME`, Codex discovery overrides and explicit
`--state-directory`, `--session-directory`, `--socket`, `--credential-directory`
and `--codex-auth-file` retain their precedence. The runtime directory must meet
the existing current-user/private-directory checks. Explicit low-level configs
use their configured registrations unless they opt into the `Launcher` section.

Bootstrap uses existing ExternalCodex resolution and Claude CLI auth/catalog
probes. It never sends model inference, installs a CLI, logs in, copies
credentials or calls Anthropic's API directly. Model/effort defaults come from
the provider catalog; no guessed static model list is used. Fresh Codex bootstrap
needs its existing selectable `models_cache.json`; a missing catalog needs an
external catalog refresh or explicit model configuration.

With one available provider it becomes the initial runtime. With two, the user
chooses once in the terminal and the choice is saved. Existing
`UNREAL_HARNESS_LLM_PROVIDER` and `UNREAL_HARNESS_LLM_MODEL` can specify that first
selection; an unavailable explicit selection fails instead of falling back.
An existing `Runtime.Provider` always remains the initial runtime. If no provider
is usable, the launcher returns safe authentication/configuration guidance and
does not publish a configuration.

The first configuration binds the current directory as workspace and uses the
existing default profile. It permits `read`, `grep` and `glob` under that
workspace, plus the Codex provider origin for transport. It denies shell/process
tools, file writes, unrestricted filesystem and unrestricted network access.
The isolated Claude protocol process is provider transport, not a grant to run
tool commands. Editing, commands and child processes require a reviewed operator
permission policy and existing child templates. Smoke profiles are never copied
into normal defaults.

If the initial working directory is the home directory/its ancestor or overlaps
known private provider, configuration or Host-state storage, bootstrap grants no
filesystem tools. Text startup still works. This metadata-only check does not
open credential files. Use a project directory or a reviewed explicit policy
for workspace inspection.

Publication uses a private temporary file, file sync and atomic no-replacement
publication, followed by directory sync. The directory is 0700 and the file 0600.
Concurrent launchers share the startup lock and consume the winning configuration;
they do not overwrite it or create duplicate Hosts/Sessions. Only nonsecret
operator configuration and credential references are saved.

## Additional providers and Team trust

Normal `unreal` startup discovers available Codex/Claude backends alongside the
configured initial provider. It preserves explicitly registered backends, even
when temporarily unavailable. Discovery augments the Host's registrations;
opening an existing Session does not replace its immutable creation identity,
applied/pending runtime selection or revision. Catalogs remain provider-scoped.

Claude's managed policy mode defaults to `reject`. Team/Enterprise requires a
one-time explicit opt-in. Add this **top-level section** to the reviewed normal
configuration without replacing `Runtime`, `Permissions` or existing registrations:

```json
"Launcher": {
  "Version": 1,
  "ClaudeCode": {
    "managedPolicyMode": "trust",
    "ToolBridge": {"Enabled": true, "Mode": "structured"}
  }
}
```

This is a section to merge into JSON, not a complete configuration. It provides
Claude discovery configuration when a Claude backend is not already explicitly
configured. Existing `Runtime.ClaudeCode` or `Providers["claude-code"].ClaudeCode`
takes precedence. Those configured backends must have their own explicit trust
setting. `Launcher.DiscoverProviders=false` disables additional discovery while
retaining configured providers and normal capability checks. `Launcher.Version`
must be 1; invalid modes/versions fail.

`managedPolicyMode=trust` means the user explicitly trusts organization-managed
Claude policy as an **external administrative boundary**. It does not mean Unreal
verified that policy is safe. Managed hooks, environment settings, telemetry,
policy helpers and runtime policy refresh may act outside Unreal's Operation
boundary. Organization-managed OpenTelemetry is left to Claude. Parent process
API/routing/OTel credentials remain excluded from Claude's restricted environment.
Unreal does not read Claude credential files, OAuth tokens or managed policy bodies.

日本語: Team/Enterprise は `managedPolicyMode=trust` を明示したときだけ登録できます。
これは「Unreal が managed policy の安全性を検証した」という意味ではなく、
「ユーザーが組織管理者の policy を外部の信頼境界として明示的に受け入れた」
という意味です。組織による hooks、env、telemetry 等の挙動は Claude 側に委ねます。
通常起動は `unreal` / `unreal my-project` だけで、smoke 用パスや unrestricted policy
をコピーする必要はありません。既存 runtime/session は勝手に書き換えません。

## Claude text and Tool Bridge capability

Normal registration evaluates tool readiness separately from Claude authentication.
New bootstrap explicitly chooses `Mode: "structured"`: it probes the official
`initialize.jsonSchema` interface with no user message/inference/tool execution
and an empty MCP configuration. MCP policy grants are irrelevant to this mode.
An old enabled bridge without `Mode`, or explicit `Mode: "mcp"`, retains SDK MCP
registration and `list_permission_rules` exact-grant checks. Modes never switch
implicitly. Schemas come from the configured tool allowlist; enabling a bridge
grants no executor permissions.

| Tool Bridge status | Normal behavior |
| --- | --- |
| `available` | Configured protocol probe succeeded; runtime denial still fails closed. |
| `blocked_by_policy` | Claude remains text-capable; no tool schemas are sent. |
| `disabled` | Bridge is disabled or no tools are permitted for exposure. |
| `unknown` | Readiness could not be established; Claude uses text-only requests. |

`/model` shows the live status, and `/analyze Overview` shows the observed
capability separately from the configured intent. JSON/Markdown analysis export
contains only safe metadata. A managed-only organization with no exact Unreal
tool grants can therefore use Claude text without making normal startup fail.
Structured mode provides Unreal-owned actions without SDK MCP grants. MCP mode
requires administrator exact grants. Both require existing executor permissions
and work through the same normal launcher without a smoke-specific configuration.

Side-effecting built-ins remain removed by `--tools ""` and safe mode. Text requests
retain deny-all/strict-empty MCP. Structured requests add only the read-only
`StructuredOutput` serializer, block all MCP and side-effecting built-ins, and use
strict-empty MCP. The serializer creates no Operation; independently validated
proposals use the Unreal Registry/Permission/Operation path. MCP requests retain
the sole attested Unreal SDK server and exact generated allowlist. A denied
StructuredOutput generation returns typed unavailable, never unsafe/provider fallback.
See [bridge modes and reviewed development profiles](claude-tool-bridge.md).

## Host reuse and Session compatibility

The launcher reuses a ready gateway only after comparing its configuration path,
session store, credential source paths and normal/explicit mode. It starts the
same binary's detached `serve` process when no owner is present, with stdin from
`/dev/null` and output in the private Host log. Gateway responses establish
readiness. Existing startup/ownership locks, stale socket handling and concurrent
startup protection remain in use.

An incompatible or older Host is not killed automatically. Stop that owning
process and relaunch with the desired configuration; keep the Session directory.
Ctrl-D and `/detach` only detach the TUI and do not stop the Host. An existing
Host retains its startup configuration until restarted.

Existing immutable Session configuration mismatch checks remain. Changing the
workspace, startup runtime, prompts or child templates can require a new Session;
editing history is never a migration mechanism. Cross-provider resume verifies
the saved nonsecret backend binding. An external catalog change that alters an
automatically discovered backend's default binding may require pinning that
backend explicitly to the saved configuration rather than silently replacing it.

## Manual verification

```sh
bin/unreal normal-smoke
# or: unreal normal-smoke
```

If this is first run with two providers, choose the initial provider once. Then:

```text
/model
/view orchestration
/analyze Overview
```

Verify provider/model/effort, bridge status and runtime revision. Choose providers
through Provider → Model → Effort; one provider skips the first step. With the
current Team policy, `blocked_by_policy` and text capability are expected until
exact grants are configured. No tool smoke is required.

Detach with Ctrl-D, then reconnect with the same normal command:

```sh
bin/unreal normal-smoke
```

The Host and conversation remain. `/view split` at sufficient width, `/analyze`,
PgUp/PgDn and `/export last` / `/export` / `/export all` retain their existing
behavior. These checks require no automatic inference. Sending a prompt is an
explicit user action; no backend request is sent by these instructions themselves.

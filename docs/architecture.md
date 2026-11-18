# Architecture

**Language:** English | [日本語](ja/architecture.md)

Unreal Agent separates **conversation & execution ownership** (the background **Host**) from **terminal presentation** (the **TUI** attach client).

---

## 1. Host and Session Ownership

The [Host](../harness/host/host.go) manages active Sessions and enforces single-writer safety:

- **One Coordinator per live Session:** Before creating or resuming a Session, the Host acquires a process-shared writer lock on the Session store.
- **Canonical Store vs. Projections:** The Session store on disk is the single source of truth. The TUI and model context views are projections derived from canonical history; provider-owned conversations are associated runtime state. Claude Code owns a separate conversation transcript and normally continues it with `--resume`.
- **Session lifetime.** Each live Session owns a context builder, provider client, tool registry, and Operation Manager. Detaching the TUI (`Ctrl+D`) or interrupting a single prompt leaves the Session running. When a parent model call fails, the failed turn ends and the Session keeps its children, their Operations, and pending approval requests. An explicit Session stop (`/stop session`) or Host shutdown drains active handlers and releases the writer lock.
- **Idempotent Submissions & Receipts:** Every user or peer input carries the current Host generation and a stable `Inbox` ID. A submission **receipt** confirms that the input was durably committed to the store (not that all downstream tool side effects have finished). Retrying with the same ID returns the original receipt; stale generations or conflicting payloads with the same ID are rejected.
- **Restart and recovery.** Pending stops are applied before Operation dispatch on resume. The reopened parent retains children whose executors still hold their writer leases. The Host reconciles abandoned children as canceled and records one end notice per child.

---

## 2. Operations, Registry, and Execution

Tool execution is split into pure validation (**Translators**) and asynchronous I/O (**Executors**):

1. **Tool Registry ([`harness/tool/registry.go`](../harness/tool/registry.go)):** Holds enabled tool definitions and their Translators (`Bash`, `ViewImage`, `SkillUse`, native file tools, AST tools, protocol tools, sub-agent tools, and the observed `claude_code` record type).
2. **Translators:** Validate model tool-call arguments and produce an immutable Operation plan without performing any filesystem, process, or network I/O.
3. **Coordinator & Operation Manager:** The Coordinator commits model responses and immutable Operation plans to canonical history, then hands execution to the Operation Manager. Executors return typed outcomes stored in `RemoteJob` handles.
4. **Observed Claude Code Operations (`claude_code`):** When Claude Code executes one of its own built-in tools, Unreal records the reported outcome as an observed receipt. An observed receipt is a historical record—it never authorizes Unreal to execute that tool a second time.

### Permission Enforcement

Permissions are enforced at the concrete execution boundary:
- With fixed policies, `host.Options.Policy` defines immutable authority (`nil` denies all).
- With interactive permission modes enabled (`/permissions`), the Host policy sets the read-only inspection baseline and the maximum allowed ceiling, while the user's selected mode (`Read Only`, `Ask for Approval`, `Full Access`) controls runtime approvals.
- Child Sessions always intersect their permissions with the parent's policy and can never widen authority. Prompt text, `AGENTS.md`, and model outputs cannot grant permissions. See [Permissions](permissions.md) and [Tools](tools.md).

---

## 3. Providers, Context, and Protocol Services

The [runtime composition](../cmd/internal/agentrunner/runtime.go) unifies both subscription providers behind the same Host:

- **Codex (`openai-codex`):** Uses Unreal's Responses API adapter and Unreal's native/AST/Bash tool pipeline.
- **Claude (`claude`):** Runs the external `claude` CLI (Claude Code), delegates tool approvals to Unreal, records CLI tool executions as observed Operations, and routes declined Bash commands through Unreal's Bash executor.
- **External Credential Ownership:** Runtime selections store only non-secret provider/model metadata. Actual OAuth tokens remain owned by the external CLIs (or Docker Sandbox proxy). There is no automatic fallback between providers.
- **LSP & DAP Boundaries:** Language Server Protocol (`harness/lsp`) and Debug Adapter Protocol (`harness/dap`) managers run as separate resource owners. LSP workspace edits go through the shared atomic mutation service. Neither LSP nor DAP state is mixed into model inference or TUI rendering.

---

## 4. TUI and Exports

The TUI is a stateless gateway client attached to the Host over a private Unix domain socket:
- Renders canonical history and live Operation state into **Chat**, **Orchestration**, or **Split** view.
- Automatically resynchronizes from the Host if a connection gap or generation change occurs.
- `/analyze`, `/copy`, and `/export` read directly from canonical public history on the Host rather than scraping terminal screen lines.
- `AGENTS.md` content is stored in canonical storage while public Host snapshots expose only its metadata (source path, byte length, SHA-256 digest).

---

## Native launcher

While daily users run the installed common-kit wrapper (`~/.local/bin/unreal`), developers working on the repository can build and run the **native launcher** directly from source.

### Building and Running from Source

1. Sign in to a provider as described in [Providers](providers.md).
2. Build the binaries from the repository root:
   ```bash
   make build
   bin/unreal --help
   ```
   `make build` produces `bin/unreal-agent`, `bin/unreal` (a symlink to `unreal-agent` that dispatches via `argv[0]`), `bin/unreal-agent-runner`, and `bin/unreal-agent-auth`.
3. Launch or attach to a named Session:
   ```bash
   bin/unreal development
   ```
   *(Unlike the common-kit wrapper, running bare `bin/unreal` without a name opens a Session named `default` instead of listing Sessions.)*

On first launch, if no runtime config exists, native bootstrap discovers available providers (preferring Claude when both are available, unless overridden by `UNREAL_HARNESS_LLM_PROVIDER` / `UNREAL_HARNESS_LLM_MODEL`), writes `runtime.json`, starts the background Host, and attaches the TUI.

### Existing Host Compatibility

The native launcher compares the configuration path, Session directory, credential directory, Codex auth path, and normal-launch binding via `launcher.check`. It does not compare executable digests or protocol revisions. This check is separate from the installed common wrapper's build/profile update detection and automatic Host restart.

### Native Launcher Flags ([`cmd/unreal-agent/launcher.go`](../cmd/unreal-agent/launcher.go))

Flags may be placed before or after the Session name. All XDG base directories must be absolute paths.

| Flag | Default & Description |
| --- | --- |
| `--config <path>` | `${XDG_CONFIG_HOME:-$HOME/.config}/unreal-agent/runtime.json`. Passing an explicit file selects an operator configuration. |
| `--state-directory <dir>` | `${XDG_STATE_HOME:-$HOME/.local/state}/unreal-agent` |
| `--session-directory <dir>` | `<state-directory>/sessions` |
| `--socket <path>` | `$XDG_RUNTIME_DIR/unreal-agent/host.sock` (if private/usable), else `<state-directory>/run/host.sock` |
| `--credential-directory <dir>` | Unset. Optional managed credential store for developer setups. |
| `--codex-auth-file <path>` | Uses standard external Codex auth discovery unless specified. |
| `--startup-timeout <dur>` | `0` (wait until canceled). Set a positive Go duration (e.g., `30s`) to bound Host readiness waiting. |
| `--bootstrap-codex-disabled` | `false`. Skip Codex during first-run bootstrap discovery. |
| `--bootstrap-claude-disabled` | `false`. Skip Claude during first-run bootstrap discovery. |
| `--bootstrap-codex-docker-oauth` | `false`. Require Docker Sandbox Codex OAuth during bootstrap. |
| `--bootstrap-codex-auto` | `false`. Auto-detect Docker Sandbox OAuth vs. external Codex login during bootstrap. |
| `--bootstrap-claude-binary <path>` | Unset. Custom path to the external `claude` executable. |
| `--help`, `-h` | Show native launcher help. |

### Runtime Configuration (`runtime.json`)

The JSON loader strictly rejects unknown fields. Top-level keys defined in [`cmd/unreal-agent/run.go`](../cmd/unreal-agent/run.go) are:
- `Runtime`, `Permissions`, `PermissionModes` (optional), `Providers`, `Launcher`, `Context`, `LanguageServers`, `DebugAdapters`, and `Subagents`.
- The Claude executable path is stored in `Runtime.Claude.Binary` / provider `Claude.Binary` (and `Launcher.ClaudeBinary` for discovery).
- Provider retry count is configured via `max_attempts`.

For low-level Host daemon flags, run `bin/unreal-agent serve -h` or `bin/unreal-agent attach -h`.

---

## Internal Package Documentation

| Package | Documentation |
| --- | --- |
| `harness/authflow` | [Authentication Flows (`unreal-agent-auth`)](../harness/authflow/README.md) |
| `harness/credential` | [Credential Store and Expiry](../harness/credential/README.md) |
| `harness/dap` | [Debug Adapter Protocol (DAP) Runtime](../harness/dap/README.md) |
| `harness/lsp` | [Managed Language Servers (LSP)](../harness/lsp/README.md) |
| `harness/permission` | [Capability and Permission Enforcement](../harness/permission/README.md) |
| `harness/profile` | [Prompt and Tool Profiles](../harness/profile/README.md) |
| `harness/provider` | [Provider Registry and Composition](../harness/provider/README.md) |
| `harness/subagent` | [Asynchronous Child Agents](../harness/subagent/README.md) |
| `harness/viewer` | [Child Session and Operation Viewer](../harness/viewer/README.md) |
| `internal/apijson` | [JSON Codec for Generated OpenAI Types](../internal/apijson/README.md) |
| `third_party/openai-openapi` | [Vendored OpenAI OpenAPI Specification](../third_party/openai-openapi/README.md) |

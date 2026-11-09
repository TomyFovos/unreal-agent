# Providers

**Language:** English | [日本語](ja/providers.md)

Unreal Agent supports **Claude only, ChatGPT (Codex) only, or both** under a single installation and `/model` menu. Both providers use your existing subscription logins. Unreal never switches providers automatically or falls back to an API key.

---

## Switching Models (`/model`)

1. Run `/model` in the TUI and choose a provider (`Claude` or `Codex`).
2. Select a model from the catalog, then choose a reasoning effort level.
3. After signing in later or updating an external CLI, run `/model refresh` to re-check authentication and reload models, then select the provider in `/model`. Reinstallation is unnecessary. An existing Session keeps its selected provider until you change it.

> **Notes:**
> - On first launch, Unreal defaults to **Claude** if both providers are ready, or whichever provider is available. (You can override the initial choice with `UNREAL_HARNESS_LLM_PROVIDER` and `UNREAL_HARNESS_LLM_MODEL`.)
> - Unavailable providers remain listed with their status and login instructions so you can see why they are disabled.
> - With neither provider ready, Unreal still starts and shows login instructions on the first input. `/login` displays the same sign-in steps.
> - Explicitly disabled providers remain disabled after login. Re-enable Codex with `bash development.sh --configure-providers --codex-auto`, or Claude with `bash development.sh --configure-providers`.
> - Seeing a model in the catalog means the CLI advertised it; your subscription tier still determines whether the provider accepts inference requests for that model.

---

## Claude (via Claude Code CLI)

### Setup

1. Install the official `claude` CLI in the Linux environment where Unreal runs.
2. Open a Bash shell, run `claude`, and execute `/login` inside Claude Code using your Claude subscription.
3. In Unreal, run `/model refresh`.

### How It Works

- **Provider ID:** `claude`
- **Conversation & tools:** Claude Code manages its own conversation state and built-in tools. Unreal launches the `claude` CLI, forwards prompts, and records completed tool calls as observed `claude_code` Operations.
- **Permissions & Bash fallback:** Every Claude Code tool call passes through Unreal's permission hook. If Claude Code declines to run a Bash command itself, Unreal executes that command through Unreal's native Bash executor under your active permission mode and returns the output to Claude.
- **Model catalog:** Populated automatically from Claude Code's initialization handshake.
- **Compaction & instructions:** See [Context and Project Instructions](context.md).

---

## Codex (Standard Subscription Route)

### Setup

1. Install the official `codex` CLI and run `codex login` in Bash with your ChatGPT subscription.
2. If Unreal reports a missing model catalog on a brand-new install, open the `codex` CLI once so it creates its local `models_cache.json`.
3. Start Unreal or run `/model refresh`.

### How It Works

- **Provider ID:** `openai-codex`
- **Execution:** Unreal sends requests through its built-in Codex Responses adapter and executes Unreal's native, AST, and Bash tools directly.
- **CLI availability:** Codex inference does not require the CLI while the selected authentication and model catalog remain valid. Use the official CLI to sign in or renew its login.
- **Credential ownership:** The external `codex` CLI owns OAuth tokens and token refresh. Unreal reads the auth file in read-only mode and never imports tokens into an internal store.
- **Auth file lookup order:**
  1. `--codex-auth-file` flag (native launcher)
  2. `OPENAI_CODEX_AUTH_FILE` environment variable
  3. `$CODEX_HOME/auth.json`
  4. `$HOME/.codex/auth.json`
  *(The auth file must be a regular private file, and `models_cache.json` is read from the same directory.)*

- **Model catalog discovery:** Standard Codex catalog discovery reads only `models_cache.json` beside the selected auth file; it never runs the CLI to enumerate models, even on `/model refresh`. Catalog availability and authentication/entitlement validation are checked independently.

---

## Docker Sandboxes Codex OAuth

If you run Unreal inside a **Docker Sandbox**, Unreal can use your host machine's OpenAI OAuth login through Docker's credential-forwarding proxy without copying tokens into the container.

### Setup

1. **On the Docker host:** Configure OpenAI OAuth for your Sandbox:
   ```bash
   sbx secret set openai --oauth
   ```
   See [Docker's Codex Sandbox documentation](https://docs.docker.com/ai/sandboxes/agents/codex/) for details.
2. **Inside the Sandbox:** Install the Unreal common kit ([Install](install.md)). The installer's default `--codex-auto` mode detects a valid Docker OAuth setup automatically, or you can require it explicitly with:
   ```bash
   bash development.sh --configure-providers --replace-pilot-launcher --docker-sandbox-oauth
   ```
3. Start Unreal and select **Codex** in `/model`.

### Technical Details & Requirements

- **Environment & config validation:** Unreal checks `SBX_CRED_OPENAI_MODE=oauth`, the non-secret auth marker file, the `sandboxd` provider in `.codex/config.toml`, and the Codex backend URL (`forced_login_method = "api"` in the Sandbox config is expected and used by Docker's proxy).
- **Forward proxy requirement:** `HTTPS_PROXY` (or `https_proxy`) must be set to `http://host.docker.internal:3128` or `http://gateway.docker.internal:3128` (conflicting uppercase/lowercase values are rejected). Docker's proxy injects the host OAuth credential in transit; Unreal never reads host tokens or installs custom root certificates.
- **Model catalog caching:**
  - First checks `models_cache.json`. If unavailable, Unreal queries `codex app-server` (`model/list`) for metadata only and caches the result at `~/.cache/unreal-agent/codex-cli-catalog.json` for 24 hours.
  - On `/model refresh` (when `models_cache.json` is unavailable), Unreal bypasses the saved CLI catalog cache to fetch a fresh list. If that lookup fails or is slow, the last successful saved catalog is retained after validating the proxy route.
  - Catalog loading does not authenticate inference and never falls back to direct network requests, another credential source, an API key, or another provider.

The Codex CLI is unnecessary when the proxy route is valid and a model catalog is already available.

---

## Delegated GPT work

When Codex is configured or enabled for discovery, Permission Modes are enabled with a configured `Maximum`, and no explicit `Subagents` configuration replaces the defaults, Unreal prepares built-in GPT templates, including for a later Codex login. Each template uses a fixed Codex model and reasoning effort independent of the parent Session's active model:

Claude is guided to use these GPT children only while Codex authentication and its model catalog are available in that Session. Otherwise it uses Claude Code's Agent tool with `haiku` or `sonnet`. A GPT parent continues to use GPT children; unavailable Codex produces login guidance and never switches its children to Claude.

| Template / Role | Model & Effort | Purpose |
| --- | --- | --- |
| `lead` | `gpt-6.1-sol` (`max`) | Coordinates multi-step implementation and can spawn its own `worker-*` or `collector` sub-agents |
| `worker-low`, `worker-medium`, `worker-high`, `worker-xhigh`, `worker-max` | `gpt-6.1-sol` (named effort) | Implements code changes and runs assigned tests |
| `collector` | `gpt-6-luna` (`medium`) | Fast read-only search, log/file extraction, CI status checks, and summaries |

### How Delegation Works

1. Ask Claude to delegate a task (or hand implementation to `lead`).
2. Claude invokes `~/.local/bin/unreal-agent subagent` via Bash, hands off the task, and ends its turn immediately.
3. When the child agent finishes or needs input, Unreal wakes the parent Session automatically.
4. Child agents inherit the parent Session's permission mode (bounded by the Host's maximum policy), and the parent agent mediates child approval requests. See [Permissions](permissions.md).

> **Environment Inheritance:** Children inherit the Host environment (including `PATH`, `HOME`, and tool configuration), excluding `OPENAI_CODEX_ACCESS_TOKEN` and `OPENAI_CODEX_ACCOUNT_ID`. This is process inheritance, not environment isolation.

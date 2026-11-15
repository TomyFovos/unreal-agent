# Providers

**Language:** English | [日本語](ja/providers.md)

Unreal Agent supports Claude and ChatGPT subscription logins, plus optional OpenAI and Anthropic API keys, under a single installation and `/model` menu. **API keys are used only when you explicitly choose an API connection and incur separate API billing.** Unreal never switches providers automatically or falls back from an expired subscription to an API key.

---

## Switching Models (`/model`)

1. Run `/model` in the TUI and choose a connection: `ChatGPT（サブスク）`, `Claude（サブスク）`, `OpenAI API（API キー・別課金）`, or `Claude（API キー・別課金）`. API rows appear only after explicit registration.
2. Select a model from that connection's catalog, then choose a reasoning effort level when available.
3. After signing in later or updating an external CLI, run `/model refresh` to re-check authentication and reload models, then select the provider in `/model`. Reinstallation is unnecessary. An existing Session keeps its selected provider until you change it.

> **Notes:**
> - On first launch, Unreal prefers a ready subscription: **Claude** if both subscriptions are ready, or the ready one. Registering an API connection does not select it. Even with only API connections registered, open `/model` to choose one, or explicitly set `UNREAL_HARNESS_LLM_PROVIDER=openai` or `claude-api` for the initial selection. `UNREAL_HARNESS_LLM_MODEL` can also select its model.
> - Unavailable providers remain listed with their status and login instructions so you can see why they are disabled.
> - With neither subscription ready, Unreal still starts. The first input shows login instructions, or directs you to `/model` when an API connection is registered. `/login` displays subscription sign-in steps.
> - Explicitly disabled providers remain disabled after login. Re-enable Codex with `bash development.sh --configure-providers --codex-auto`, or Claude with `bash development.sh --configure-providers`.
> - Model visibility does not guarantee inference access: the selected subscription or API account determines model access. Listing connections in `/model` does not request an API catalog or run inference.

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

## Optional API Key Connections

Prepare key files with permissions **0600**, then register only the connections you want:

```bash
chmod 600 /absolute/openai.key /absolute/anthropic.key
bash development.sh --openai-api-key-file /absolute/openai.key --anthropic-api-key-file /absolute/anthropic.key
```

Each flag is optional. OpenAI API (`openai`) uses Unreal's existing Responses adapter and does not require a Codex subscription login. Claude API (`claude-api`) requires the official Claude Code CLI and uses the same tools and permission handling as subscription Claude.

- **Explicit source:** Settings contain only a file path, or an environment variable name explicitly chosen with `--openai-api-key-env NAME` / `--anthropic-api-key-env NAME`. Merely setting `OPENAI_API_KEY` or `ANTHROPIC_API_KEY` never registers or selects an API connection. Key values are not written to configuration, logs, history, UI, or errors. Key files are reread when used; files without permissions 0600 are rejected.
- **Selection:** After registration, choose the API row in `/model`. A Session retains its selected connection on resume and never switches between subscription and API authentication automatically. API labels in `/model` and status displays identify separate billing.
- **Models:** After you open the OpenAI API connection, Unreal reads its [`GET /v1/models` catalog](https://developers.openai.com/api/reference/resources/models/methods/list). That response does not advertise reasoning effort levels, so Unreal omits the optional effort setting. Claude API models come from the selected Claude Code initialization handshake.
- **Claude authentication:** `claudeagent` passes `ANTHROPIC_API_KEY` only for the selected API route; the subscription route continues to remove API key and auth-token variables. With Claude Code **2.1.322**, a fake API key alongside fake subscription OAuth produced `init.apiKeySource=ANTHROPIC_API_KEY` and an `x-api-key` request to a local fake server. No real inference was used. Additional settings flags are unnecessary; empty `--setting-sources` does not isolate OAuth, and `--bare` also changes instruction loading, so Unreal uses neither for this route.
- **Usage:** Response `Usage.AuthMethod` and `claude_usage` connection metadata distinguish API keys from subscriptions and retain token counts. API cost calculation is not included.

For an operator-managed `runtime.json`, the equivalent nonsecret references are:

```json
{
  "Launcher": {
    "Version": 1,
    "APIKeys": {
      "openai": {"file": "/absolute/openai.key"},
      "claude-api": {"file": "/absolute/anthropic.key"}
    }
  }
}
```

This fragment registers connections; it does not change the Session's active selection. Use `{"env": "CHOSEN_KEY_NAME"}` instead of `file` when explicitly choosing an environment source.

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
3. Start Unreal and select **ChatGPT（サブスク）** in `/model`.

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

When Codex is configured or enabled for discovery, or an OpenAI API connection is explicitly registered, Unreal prepares built-in GPT templates if Permission Modes have a configured `Maximum` and no explicit `Subagents` configuration replaces the defaults. Template models are independent of the parent Session's active model.

An **OpenAI API** parent uses `openai` for its built-in GPT children and their workers. Other parents retain the `openai-codex` subscription route. Availability never changes that choice. API children keep the template models but omit effort because the API catalog does not advertise it; the subscription efforts below remain unchanged. Explicit operator templates and existing children retain their recorded connections.

Claude is guided to use GPT children only while Codex authentication and its model catalog are available. Otherwise it uses Claude Code's Agent tool with `haiku` or `sonnet`. An unavailable selected GPT connection produces guidance without switching to another connection.

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

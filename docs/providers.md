# Providers

**Language:** English | [日本語](ja/providers.md)

Unreal Agent supports Claude and ChatGPT subscription logins, optional OpenAI and Anthropic API connections, and OpenCode Go under a single installation and `/model` menu. **Connections authenticated with API keys are used only after explicit selection.** OpenAI and Anthropic API connections incur separate API billing; OpenCode Go uses its subscription key. Unreal never switches providers automatically or falls back from an expired subscription to an API key. See [OpenCode Go](#opencode-go-api-key).

---

## Switching Models (`/model`)

1. Run `/model` in the TUI and choose a connection: `ChatGPT（サブスク）`, `Claude（サブスク）`, `OpenAI API（API キー・別課金）`, or `Claude（API キー・別課金）`. `OpenCode Go` appears after its key is saved. API connections require explicit registration.
2. Select a model from that connection's catalog, then choose a reasoning effort level when available.
3. After signing in later or updating an external CLI, run `/model refresh` to re-check authentication and reload models, then select the provider in `/model`. Reinstallation is unnecessary. An existing Session keeps its selected provider until you change it.

> **Notes:**
> - On first launch, Unreal prefers a ready Claude or ChatGPT login: **Claude** if both subscriptions are ready, or the ready one. Registering an API connection does not select it. Even with only API connections registered, open `/model` to choose one, or explicitly set `UNREAL_HARNESS_LLM_PROVIDER=openai` or `claude-api` for the initial selection. `UNREAL_HARNESS_LLM_MODEL` can also select its model.
> - Unavailable providers remain listed with their status and login instructions so you can see why they are disabled.
> - With neither subscription ready, Unreal still starts. The first input shows login instructions, or directs you to `/model` when an API connection is registered. `/login` displays subscription sign-in steps.
> - Explicitly disabled providers remain disabled after login. Re-enable Codex with `bash development.sh --configure-providers --codex-auto`, or Claude with `bash development.sh --configure-providers`.
> - Model visibility does not guarantee inference access: the selected subscription or API account determines model access. Listing connections in `/model` does not run inference. OpenAI API catalogs are requested only after choosing that connection; Go discovery reads its public catalog without a key.

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

## OpenCode Go (API key)

Unreal can add OpenCode Go as an additional provider with an API key saved in the managed credential store. It connects only to the OpenCode Go subscription endpoint (`https://opencode.ai/inference/go/openai/v1`, Responses API), never the pay-as-you-go Zen endpoint. Unreal offers the 24 models the fork's live survey verified through that endpoint, DeepSeek V4.1 Flash (`deepseek-v4.1-flash`) first. MiMo-V2.6-Flash, the two Contributor models (prompts are used for training) and the Free models are not offered.

### Setup

1. Save the key in the managed store (`~/.local/state/unreal-agent/credentials`, provider `opencode-go`, id `primary`) with `unreal-agent-auth login ... --key-stdin`. `/login` in the TUI shows the exact command for your installation.
2. Run `/model refresh`. `/model` then lists `OpenCode Go · available`; no Host restart is needed.
3. Choosing it in `/model` switches only the conversation's own model. Pick a model from the 24; models that list reasoning efforts then ask for the effort, and models whose list names none skip that step.

### How It Works

- **Provider ID:** `opencode-go`
- **Registration:** No config is needed. The provider is registered only while a key is saved in the managed store; without one nothing changes, `/model` does not list it, and nothing is sent to OpenCode.
- **Selection:** Saving a key only registers the connection. Choose OpenCode Go in `/model` or `/model-sub`, or set `UNREAL_HARNESS_LLM_PROVIDER=opencode-go` explicitly at first launch. It is excluded from automatic initial selection.
- **Endpoint:** Requests go only to the Go subscription endpoint above. Unreal never switches to Zen, to another provider, or to an API key automatically: if the selected model is unavailable, the request fails.

Full details: [OpenCode Go](opencode-go.md) (Japanese).

---

## Delegated GPT work

When Codex is configured or enabled for discovery, or an OpenAI API connection is explicitly registered, Unreal prepares built-in GPT templates if Permission Modes have a configured `Maximum` and no explicit `Subagents` configuration replaces the defaults. Template models are independent of the parent Session's active model.

Without a saved `/model-sub` choice, an **OpenAI API** parent uses `openai` for its built-in GPT children and their workers. Other parents retain the `openai-codex` subscription route. Availability never changes that choice. API children keep their template models and configured effort; the subscription defaults below remain unchanged. Explicit operator templates and existing children retain their recorded connections.

Claude is guided to use GPT children only while Codex authentication and its model catalog are available. Otherwise it uses Claude Code's Agent tool with `haiku` or `sonnet`, or the Claude model chosen with `/model-sub`. An unavailable selected GPT connection produces guidance without switching to another connection.
With OpenCode Go selected for sub-agents, availability follows its registered key; the catalog is read when a child starts. Explicit Claude-model work through Agent remains available alongside GPT children.

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

### Choosing the sub-agents' model (`/model-sub`)

`/model-sub` chooses the model sub-agents use: **OpenAI Codex** (the subscription connection, shown as `ChatGPT（サブスク）`), **OpenCode Go** then one of the 24 verified models (DeepSeek V4.1 Flash first), or **Claude**, which appears when Claude is registered (the same provider list `/model` shows) and then offers the same Claude models `/model` lists. The choice is stored in `STATE/subagent-model.json`, applies to every Session using the installation's shared state, and takes effect for children started afterwards; running children keep their model. Children started with an explicit runtime (`/children start ...`) and explicit `Subagents` templates or `SubagentDefaults` in the config file are not affected. `subagent start --model ID --effort LEVEL`, which changes only a template's model and effort and keeps its provider, works on the built-in templates only while `/model-sub` is OpenAI Codex: with OpenCode Go or Claude selected it is refused (`override_unavailable`) rather than run on Codex, and Claude's instructions leave it out.

Without a saved choice, an OpenAI API parent keeps its explicitly selected API connection for built-in GPT children; other parents use Codex. Saving **OpenAI Codex** explicitly pins the subscription route, including for an API parent. An unavailable explicit choice fails without using the parent's API connection.

With Claude selected, Claude Code's Agent tool runs the sub-agents on the chosen model instead of Unreal's children: Unreal sets `CLAUDE_CODE_SUBAGENT_MODEL` and `CLAUDE_CODE_SUBAGENT_MODEL_FORCE=1` in Claude Code's launch environment, and the delegation prompt names that model instead of `haiku` or `sonnet`. The force variable puts every Agent sub-agent, the built-in Explore and Plan included, on the chosen model and leaves the Agent tool without a per-call model, so Claude cannot pass another model per call; it needs Claude Code 2.1.257 or later. Per Claude Code's docs, a fork and a skill that runs in a sub-agent with `model: inherit` stay on Claude's own model. Agent uses the parent Session's selected Claude authentication; choosing a child model does not switch between subscription and API-key connections. A Claude parent gets no Unreal children, so the choice applies from Claude's next turn and running children keep their model. A parent on Codex or OpenCode Go that starts a built-in child template is refused with `provider_unavailable` and a hint to choose OpenAI Codex or OpenCode Go with `/model-sub`; Unreal never switches to another provider. Whether the environment variables reach Claude Code's Agent sub-agents on the real CLI, and whether their tool calls show in the TUI, is unconfirmed until a real check.

With OpenCode Go selected, every template runs the model chosen in `/model-sub`. For DeepSeek V4.1 Flash the mapping is unchanged:

| Template | Effort |
| --- | --- |
| `lead`, `worker-max` | `max` |
| `worker-xhigh`, `worker-high`, `worker-medium` | `high` |
| `worker-low`, `collector` | `low` |

The effort for each role comes from the selected model's list: `lead` and `worker-max` take its highest effort, `worker-low` and `collector` its lowest, and the other workers their own effort when the model has it, otherwise the model's default (or, when that default is below their own, the highest effort the model offers below their own; `qwen3.8-max` runs `worker-high` at `medium`). Models whose list names no reasoning effort run without one. If the selected model is unavailable (limit reached, outage), the child start or request fails; Unreal never switches to Codex or another provider automatically. With OpenCode Go selected, Claude is offered the children while OpenCode Go is registered and its key is saved, instead of while Codex is available; the model list is read when a child starts. The templates are kept ready before a Codex login when provider discovery is on (the default), so a later choice applies from the next child start. Only on a Host with Codex disabled or discovery off, where OpenCode Go was not selected when the Host started, there are no templates: the choice is saved and used from the next Host start (`/model-sub` says so).

### Distributor-disabled OpenCode Go

A distribution built with `disable_opencode_go` omits the entire OpenCode Go connection from `/model`, `/model-sub`, discovery, model lists and auth methods. Environment variables, old configuration, keys and saved Session selections cannot enable it. Existing Sessions retain their choice, display why it is unavailable, and ask you to choose another connection with `/model`. Children fail with guidance to use `/model-sub`; no automatic fallback occurs. Check `--version` for `opencode_go_enabled=false`. See [disabled distribution builds](distribution.md#distribution-builds-without-opencode-go).

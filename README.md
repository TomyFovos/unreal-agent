# Unreal Agent

**Language:** English | [日本語](docs/ja/README.md)

Unreal Agent is a terminal coding agent for day-to-day development with **Claude only**, **ChatGPT (Codex) only**, or **both** in a single interface.

This repository is a personal-use fork of [unreallabsai/unreal-agent](https://github.com/unreallabsai/unreal-agent). It preserves Unreal's background Host/Session architecture and canonical Operation model while adding:
- Subscription-based provider switching (`/model`) between Claude Code and Codex
- Interactive permission modes (`Read Only`, `Ask for Approval`, `Full Access`)
- Delegated GPT sub-agents (`lead`, `worker-*`, `collector`), or Claude Code's Agent (`haiku` / `sonnet`) when Claude cannot use Codex

## Key Characteristics

- **Subscription-only authentication:** Claude runs through the signed-in `claude` CLI (Claude Code). Codex uses your ChatGPT subscription via `codex login` (or Docker Sandbox OAuth). One login is enough to work; with neither login, Unreal starts and shows sign-in instructions on your first input. Sign in later, run `/model refresh`, and select the provider in `/model`; reinstalling is unnecessary.
- **No silent fallbacks:** Unreal never switches providers automatically or falls back to paid API keys.
- **Persistent background sessions:** A background Host process owns the conversation history. Detaching from the terminal (`Ctrl+D`) leaves your Session running so you can reattach at any time.

## Quick Start

1. **Sign in to a provider (one is enough):**
   - **Claude:** Run `claude` in Bash, then run `/login` inside Claude Code.
   - **Codex:** Run `codex login` in Bash.
2. **Install the Linux amd64 kit:** Follow the [Installation Guide](docs/install.md).
3. **Start or attach to a session:** In your project directory, run:
   ```bash
   unreal development
   ```
   Enter `y` if prompted to create a new Session.
4. **Choose model and permissions:**
   - Run `/model` to select a provider, model, and reasoning effort.
   - Run `/permissions` to select `Read Only`, `Ask for Approval`, or `Full Access`.
5. **Send a task:** Type your prompt and press `Enter`. Press `Ctrl+D` to detach; run `unreal development` again to reconnect.

New installations start with Claude when both providers are available, or the available provider when only one is ready. A GPT parent uses GPT children; if Codex becomes unavailable, Unreal shows login guidance without switching to Claude children. See [Providers](docs/providers.md) for availability and explicitly disabled providers.

> **Note:** On terminals 120 columns or wider, Unreal opens in **Split** view (Chat on the left, live agent/tool cards on the right). Narrower terminals open in **Chat** view.
>
> **Security Note:** `Full Access` skips per-operation approval prompts inside Unreal, but it is **not** an OS sandbox. Run Unreal inside a container or VM if you need operating-system isolation.

## Documentation

### User Guides

| Document | Topics Covered |
| --- | --- |
| [Installation](docs/install.md) | System requirements, login setup, kit installation, Docker Sandbox transfer, and updates |
| [Usage](docs/usage.md) | Session lookup, TUI views, keyboard shortcuts, copy/export, stopping/resuming, and sub-agents |
| [Providers](docs/providers.md) | Claude Code, Codex subscription, Docker Sandbox OAuth, model catalogs, and delegated GPT roles |
| [Permissions](docs/permissions.md) | Permission modes, approval prompts, persistence across restarts, and filesystem boundaries |
| [Troubleshooting](docs/troubleshooting.md) | Fixing login expiration, startup issues, missing models, reconnecting, logs, and `/analyze` |

### Developer & Architecture Guides

| Document | Topics Covered |
| --- | --- |
| [Contributing](CONTRIBUTING.md) | Build prerequisites, test workflow, `make prepush`, and architectural invariants |
| [Architecture](docs/architecture.md) | Host/Session ownership, Operation execution pipeline, provider routing, and native launcher flags |
| [Context & Instructions](docs/context.md) | Codex context projection, tool-result pruning, summarization, Claude compaction, and `AGENTS.md` |
| [Tools & File Mutation](docs/tools.md) | Native file tools, Tree-sitter AST search/edit, and atomic file mutation guarantees |
| [Distribution](docs/distribution.md) | Building snapshots and common kits, GoReleaser setup, CI/Release workflows, and release verification |
| [TUI Design Specification](docs/design/UNREAL_AGENT_TUI_DESIGN.md) | Complete visual, layout, responsive, and state specification for the terminal UI |
| [Headless Runner](cmd/unreal-agent-runner/README.md) | Running single prompts or JSON requests with JSONL output for evaluation |
| [Harbor Adapter](benchmarks/harbor/README.md) | Running Harbor and Terminal-Bench evaluations |

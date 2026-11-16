# Authentication Flows (`harness/authflow`)

**Language:** English | [日本語](../../docs/ja/harness/authflow/README.md)

This package manages developer/evaluation authentication methods and powers the `unreal-agent-auth` CLI.

> **Note:** Daily interactive TUI usage relies on external subscription logins (`claude` and `codex`). Managed credential storage here is for developer and evaluation workflows.

---

## 1. Supported Authentication Matrix (Reviewed 2026-09-27)

| Provider / Method | Status | Credential Owner & Inference Path |
| --- | --- | --- |
| **OpenAI API key** | Supported | Managed in harness private store; uses built-in Responses adapter |
| **OpenCode Go API key** | Supported | Harness private store; explicitly selected OpenCode Go Responses adapter |
| **Anthropic API key** | Configuration only | Stored in harness store; requires an explicitly registered Anthropic adapter for inference |
| **Claude.ai Pro/Max OAuth** | Unsupported | Direct third-party OAuth mediation is not permitted by Anthropic policy |
| **OpenAI Codex direct OAuth** | Deferred | Uses read-only external CLI credentials (`~/.codex/auth.json` or Docker Sandbox proxy) |
| **GitHub Copilot OAuth** | Deferred | Requires a separate runtime/adapter contract |

### Official Policy & Documentation References

- [OpenAI API Key & Responses Quickstart](https://developers.openai.com/api/docs/quickstart)
- [Codex Authentication](https://learn.chatgpt.com/docs/auth)
- [Anthropic API Authentication](https://platform.claude.com/docs/en/api/overview)
- [OpenCode Go](https://opencode.ai/docs/go/)
- [Claude Code Credential Policy](https://code.claude.com/docs/en/legal-and-compliance#authentication-and-credential-use)
- [GitHub Copilot SDK Authentication](https://docs.github.com/en/copilot/how-tos/copilot-sdk/auth/authenticate)

---

## 2. `unreal-agent-auth` CLI Usage

Build the binary:

```bash
go build ./cmd/unreal-agent-auth
```

### Subcommands

Flags must be placed **after** the subcommand:

```bash
# List supported authentication methods
unreal-agent-auth methods

# Store an API key from a non-interactive pipe/file (terminal stdin is rejected to prevent echo)
unreal-agent-auth login --store /private/path --provider openai --id work --method api_key --key-stdin < /path/to/keyfile

# List stored credential metadata (never prints secret values)
unreal-agent-auth list --store /private/path

# Remove a stored credential
unreal-agent-auth logout --store /private/path --provider openai --id work --method api_key
```

### Security & Runtime Rules

- **No secret flags or terminal echo:** Keys must be piped via `--key-stdin` from a non-TTY source.
- **No Inbox/Transcript leakage:** `LoginRequest` is handled directly by `authflow.Service` and never passes through Session `Inbox`, `Operation`, or transcript logs. Only non-secret `Metadata` is returned.
- **Using managed credentials in `unreal-agent-runner`:** Set `UNREAL_HARNESS_CREDENTIAL_DIRECTORY` and `UNREAL_HARNESS_CREDENTIAL_ID` alongside your explicit provider/model selection. When a managed credential ID is configured, the runner will not fall back to environment API keys.
- **No speculative inference check on login:** Local login success means the key was installed, not that inference entitlement was verified; no speculative model request is sent during login.
- **Preflight method validation:** Call `ValidateMethod` before requesting a secret so unsupported authentication methods are rejected before secret input.

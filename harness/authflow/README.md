# Authentication support (reviewed 2026-09-27)

| Provider / method | Status | Owner / inference |
|---|---|---|
| OpenAI API key | Supported | Harness private store; existing Responses adapter |
| Anthropic API key | Configuration supported | Harness store; embedding must install an Anthropic adapter explicitly |
| Claude.ai Pro/Max OAuth | Unsupported | Third-party mediation is not permitted |
| OpenAI Codex direct browser/device OAuth | Deferred | Existing read-only external credentials remain externally owned |
| GitHub Copilot OAuth / inference | Deferred | An SDK login does not establish adapter compatibility without a second runtime |

Official sources:
- [OpenAI API key and Responses quickstart](https://developers.openai.com/api/docs/quickstart)
- [Codex authentication](https://learn.chatgpt.com/docs/auth)
- [Anthropic API authentication](https://platform.claude.com/docs/en/api/overview)
- [Claude Code credential policy](https://code.claude.com/docs/en/legal-and-compliance#authentication-and-credential-use)
- [Copilot SDK authentication](https://docs.github.com/en/copilot/how-tos/copilot-sdk/auth/authenticate)

The Codex docs describe native login and credential handling. They do not by
themselves establish a direct third-party OAuth client registration/exchange/
refresh contract. Copilot documents OAuth application tokens, but integrating
its inference runtime without duplicating the harness loop/session/tool owner
remains unestablished. No private endpoint or borrowed OAuth client is enabled.

## CLI and UI

Build with: go build ./cmd/unreal-agent-auth

Commands (flags follow the subcommand):
- methods
- login --store /private/path --provider openai --id work --method api_key --key-stdin
- list --store /private/path
- logout --store /private/path --provider openai --id work --method api_key

Supply login input through a private redirected pipe/file. Secrets are not flag
values; terminal stdin is rejected to avoid echo. Login validates and stores
the key locally; it does not claim that an account has inference entitlement.
The next explicit model request validates that at the provider.

A TUI calls authflow.Service from its masked credential-entry surface. Do not
route LoginRequest through Inbox, Operation, Session or transcript. Return only
Metadata. Unsupported methods return credential.Error(unsupported_auth_flow).

To use managed storage in the runner set UNREAL_HARNESS_CREDENTIAL_DIRECTORY and
UNREAL_HARNESS_CREDENTIAL_ID, and select the provider/model explicitly as usual.
No environment-key fallback occurs when a managed reference is configured.
An Anthropic key can be represented/stored, but inference remains unsupported
until an explicit compatible adapter is registered.

## Validation and limits

Tests exercise CLI login -> private Store -> Resolver -> provider.Registry ->
existing OpenAI Responses adapter -> normalized tool-call result, followed by
logout and rejection, with a local provider protocol server. They cover forbidden
entitlement, cancellation, transport denial and redaction. No second agent loop
runs, and tool calls remain outputs for the Coordinator.

Live provider credentials were not used. No three-provider subscription OAuth
completion is claimed. Browser PKCE/state and device slow_down/expiry tests are
not applicable because no browser/device flow is enabled. Generic refresh,
rotation, crash/persist uncertainty and cross-process coordination are covered
by credential tests; API keys do not refresh automatically.

# Docker Sandbox Codex OAuth proxy

Docker's Codex kit supports ChatGPT subscription OAuth owned by the Docker
host. `forced_login_method = "api"` is a Codex CLI routing setting; it does
not mean that the subscription has become an API-key account.

The pinned [kit specification](https://github.com/docker/sbx-kits-contrib/blob/eeaf96617172b3544e846378e6f7dca73c9db6c8/codex/spec.yaml)
sets `SBX_CRED_OPENAI_MODE=oauth`, `model_provider="sandboxd"`, and
`requires_openai_auth=false`. Its `auth.json` has only
`OPENAI_API_KEY: "proxy-managed"`; the selected TOML provider has the public
access sentinel `oai-oat01-proxy-managed` and the exact endpoint
`https://chatgpt.com/backend-api/codex`. The host proxy substitutes its OAuth
credential for this sentinel. Unreal does not access that host credential.
See [Docker credential isolation](https://docs.docker.com/ai/sandboxes/configuration/credentials/).

## Explicit route

Choose this authentication reference in an operator runtime configuration:

```json
{"provider":"openai-codex","id":"docker-sandbox-oauth","method":"docker_sandbox_oauth"}
```

This is separate from native `external-codex` / `oauth`. Native discovery
never infers the proxy route from a placeholder. Optional normal launcher
discovery can explicitly opt in with `Launcher.CodexDockerSandboxOAuth=true`.
Existing Session startup identities are not migrated. Use a new Session and
an isolated Pilot config/state/socket for this route.

Non-inference preflight (path only, no credential values on stdout):

```sh
unreal-agent-auth check-docker-sandbox-oauth --codex-auth-file "$CODEX_HOME/auth.json"
```

The existing file-path resolution (`--codex-auth-file`,
`OPENAI_CODEX_AUTH_FILE`, `CODEX_HOME`, `HOME/.codex`) remains unchanged.
Before every generation, the resolver verifies the kit mode, exact JSON
marker, real TOML syntax, selected provider, exact endpoint/sentinel and
`requires_openai_auth=false`. Additional provider credential/header settings
are rejected. Kit source files are bounded regular files, not symlinks or
group/world-writable files. Readable 0644 files are permitted **only** for this
nonsecret marker/config; native token files still require private permissions.

The resolver returns an owner marker with **no token or account ID**. The
protocol adapter emits only the fixed public sentinel, omits
`ChatGPT-Account-ID`, rejects alternate URLs and unexpected headers, disables
cookies and redirects, and does not enable ambient HTTP proxies. The existing
Permission transport still authorizes every exchange. Docker's transparent
host proxy remains responsible for OAuth substitution; network reachability
inside a particular Sandbox requires operator verification.

No API-key fallback, dummy account ID, token extraction, auth-file copying,
managed token store, OAuth refresh, permission import, foreign MCP invocation
or provider fallback is implemented. The CLI kit's `danger-full-access` and
MCP configuration do not become Unreal permissions. Source inconsistency is
`credential: docker_sandbox_oauth_unavailable`; proxy 401/403 remains a typed
provider failure, without exposing response bodies or retrying authorization.

## Verification

Unit/Host fixtures cover an OAuth proxy exchange, exact endpoint/headers,
API-key-mode refusal, duplicate/malformed configuration, reread between
generations, mixed native/proxy material, unauthorized responses, cancellation,
Permission denial, startup and `/model` catalog access. All fixture tokens and
accounts are dummy values; tests do not contact Docker or a real provider.
Sandbox connection and real inference are deliberately left to the operator.

For `/model`, the existing CLI cache is preferred. If it is missing/incompatible,
this explicit proxy route can use a bounded metadata-only `model/list` process;
see [Codex model catalog](codex-model-catalog.md). It does not run inference,
create threads, import credentials or grant tool permissions.

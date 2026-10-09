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
unreal-agent-auth check-docker-sandbox-oauth --codex-auth-file "$CODEX_HOME/auth.json" --check-forward-proxy
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
`ChatGPT-Account-ID`, rejects alternate URLs and unexpected headers, and disables
cookies and redirects. Docker's **forward** proxy performs OAuth substitution;
the transparent path enforces network policy but **does not inject credentials**.
Disabling the forward proxy sends the public sentinel to the backend unchanged,
producing a 401 even when the original Codex CLI successfully authenticates.
See [Docker network isolation](https://docs.docker.com/ai/sandboxes/security/isolation)
and [credential-proxy troubleshooting](https://docs.docker.com/ai/sandboxes/troubleshooting/#agent-authentication-failure).

Only this explicitly selected authentication route reads `HTTPS_PROXY` (or
`https_proxy` when uppercase is absent). Standard HTTP transports require one
of the exact public Docker listener URLs `http://host.docker.internal:3128` or
`http://gateway.docker.internal:3128`. Conflicting case variants, missing values,
credentials/userinfo, alternate schemes/hosts/ports, paths, queries and fragments
are rejected without a request. The port is Docker's documented forward listener;
the two Docker-owned host/gateway aliases are described in
[Docker's Sandbox architecture](https://www.docker.com/ja-jp/blog/untrusted-autonomous-workload-ai-sandboxes/)
and [Docker networking](https://docs.docker.com/desktop/features/networking/networking-how-tos/).
No proxy URL is stored in runtime identity or Session history. `NO_PROXY`,
`HTTP_PROXY`, `ALL_PROXY` and corporate/system proxies cannot redirect or bypass
this route. The runtime's existing environment is not rewritten.

The proxy is pinned in a private transport clone, with CONNECT credentials and
custom CONNECT headers removed. CONNECT targets only `chatgpt.com:443`; the
application request still targets only the exact HTTPS `/backend-api/codex/responses`
endpoint. TLS certificate verification uses the existing trust configuration;
it is never disabled and no CA is installed by Unreal. Native ExternalCodex and
its transport/authentication behavior remain unchanged. A trusted custom host
RoundTripper remains injectable for fixtures, under the same endpoint/header guard.
The existing Permission transport still authorizes every backend exchange; the
fix does not add an origin or grant network/process/filesystem permissions.
Children receive only the validated public Docker proxy URL, never arbitrary
parent proxy settings, bypass rules or proxy credentials.

Missing/unrecognized inference routing is the safe typed error
`provider: proxy_unavailable`. The optional non-inference preflight flag reports
`credential: docker_sandbox_proxy_unavailable` before the Pilot starts. Neither
path falls back to direct traffic or another provider. A nonstandard listener
needs separate review instead of expanding the allowlist automatically.

No API-key fallback, dummy account ID, token extraction, auth-file copying,
managed token store, OAuth refresh, permission import, foreign MCP invocation
or provider fallback is implemented. The CLI kit's `danger-full-access` and
MCP configuration do not become Unreal permissions. Source inconsistency is
`credential: docker_sandbox_oauth_unavailable`; proxy 401/403 remains a typed
provider failure, without exposing response bodies or retrying authorization.

## Verification

Model continuation failures retain safe `stage`, numeric HTTP `status`, a closed
`reason` and `retryable` in the public typed provider error. HTTP rejection and
an API error delivered inside a successful SSE response have distinct stages
(`http_status` and `stream_error`). Transport categories are captured before
primitive error strings erase Go types; arbitrary upstream error codes, bodies,
URLs, headers and request IDs are discarded at the provider boundary. No request
body is logged or persisted by this diagnostic. The existing retry policy and
attempt limit remain unchanged. Retry eligibility never authorizes replay of a
completed Operation, an authentication switch or a provider fallback.

`remote_failure` alone from older binaries does not establish a malformed tool
result or a transient upstream incident. The loopback forward-proxy Host fixture
checks Bash/create/read/edit/read/Bash continuation, the actual file bytes,
canonical receipts, injected HTTP 502/SSE errors and explicit resume. Completed
Operations and history remain unchanged on resume; human Full Access expires.
Sandbox incident classification still needs the safe fields from the failing
request; a local fixture is not evidence of a real Sandbox's HTTP response.

Unit/Host fixtures cover a real loopback CONNECT/TLS proxy exchange, exact endpoint/headers,
API-key-mode refusal, duplicate/malformed configuration, reread between
generations, mixed native/proxy material, 401/403 responses, cancellation,
Permission denial, mandatory relay/no-direct-fallback, startup, durable Final,
resume/input deduplication and `/model` catalog access. All fixture tokens and
accounts are dummy values; tests do not contact Docker or a real provider.
Sandbox connection and real inference are deliberately left to the operator.

For `/model`, the existing CLI cache is preferred. If it is missing/incompatible,
this explicit proxy route can use a bounded metadata-only `model/list` process;
see [Codex model catalog](codex-model-catalog.md). It does not run inference,
create threads, import credentials or grant tool permissions.

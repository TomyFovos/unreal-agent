# Codex model catalog

`/model` reads nonsecret Codex CLI metadata; it never sends a prompt, starts a
thread/turn, authorizes a tool, creates an Operation or changes authentication.
Model IDs and efforts come from the CLI, never a hard-coded model list.

## Sources and compatibility

The existing `models_cache.json` beside the resolved `auth.json` remains first.
Path resolution still uses explicit `--codex-auth-file`,
`OPENAI_CODEX_AUTH_FILE`, `CODEX_HOME`, then `HOME/.codex`. Cache identity,
instructions and unrelated fields are excluded. Hidden models and reasoning
levels the harness cannot encode are omitted; the effort picker explains the
latter. A cache format failure never becomes a guessed list.

Only the explicit `docker_sandbox_oauth` route can use a metadata-only
`codex app-server --listen stdio://` when that cache is missing or incompatible.
Native WSL ExternalCodex retains its cache-only source. A redirected/nonregular
or oversized cache is refused without handing it to app-server.

The reader validates the existing Docker OAuth route, keeps the same selected
Codex home/provider configuration, and sends exactly `initialize`,
`initialized` and paginated `model/list` (`includeHidden=false`). No auth,
thread, turn, process, filesystem or tool RPC is sent. Server execution/approval
requests and unknown notifications are rejected. The documented non-execution
notifications `account/updated`, `account/rateLimits/updated`, `configWarning`
and `remoteControl/status/changed` are discarded without retaining their
contents. Initialization suppresses automatic gateway authorization.

CLI stdout is bounded to 4 MiB, each frame to 1 MiB, 64 frames, 8 pages and 256
models; the process deadline is at most 15 seconds (default 10). Process groups
are terminated on completion/cancel/timeout. CLI runtime DB/log files are placed
in a fresh private temporary directory, not the existing Session Store. Its
environment includes only HOME/PATH/CODEX_HOME/NO_COLOR and the closed OAuth
mode marker. Process-local overrides disable hooks, shell tools, apps and remote
control; they do not rewrite Codex configuration. API keys, actual tokens, proxy env and other-provider credentials
are not forwarded; auth files are never copied or modified.

The process result/failure has a one-minute in-memory TTL; `/model refresh`
invalidates it. Cache files are reread on each open/confirmation. The same reader
supplies Host discovery and runtime selection validation. A picker failure
shows a closed reason (missing cache/CLI, timeout, incompatible protocol, invalid
metadata, request rejected, invalid OAuth source) and retains the current model
and effort. Raw stderr/error bodies, identities and arbitrary Problem text are
not surfaced.

The official [model/list contract](https://learn.chatgpt.com/docs/app-server#list-models-modellist)
defines model-specific effort options. The locally installed CLI 0.160.0's
generated `v2/ModelListParams`, `v2/ModelListResponse`, `v2/Model` and
`ServerNotification` types were also checked. Docker's custom provider can
return the CLI's bundled/cached catalog: this is metadata, **not an account
entitlement or successful inference check**.

## Verification and asp update

Unit/fake CLI fixtures cover missing/incompatible cache, protocol failures,
pagination/limits, supported efforts, restricted environment, cancellation,
refresh/TTL, auth/config preservation and nonsecret errors. Host fixtures prove
selection/effort validation with zero inference/Operations and unchanged auth.
An opt-in installed-CLI test uses a synthetic HOME/provider fixture only:

```sh
UNREAL_CODEX_CATALOG_BINARY="$(command -v codex)" go test ./harness/modelcatalog -run '^TestCodexInstalledCLIModelListOnly$' -v
```

The asp kit uses a fresh `--instance oauth-catalog-v1` so an already-running old
binary cannot be reused. Existing `oauth-v1` configs, Host and Session history
remain untouched; no migration occurs. Follow the kit README to activate and
run `unreal asp-catalog`, then `/model`. Sandbox authentication/connectivity and
actual model entitlement remain an operator check; this task sends no real
inference and does not connect to asp or alter devlore.

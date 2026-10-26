# Local snapshot distribution

The current common development kit uses the **same archive and setup** in both
asp and devlore. From its extracted directory, run:

```sh
bash development.sh --configure-providers --replace-pilot-launcher
```

It asks independently for the Codex authentication route and this environment's
Claude policy audit/trust. Native `unreal-agent providers` performs only auth and
catalog preflight; unsupported/unavailable providers retain closed status codes.
No model inference, credential import or tool execution occurs during setup.
Use `/model` for provider/model/effort and `/permissions` for human authorization.

After a settings/build change, opening an existing Git workspace starts a
**new Session in a separate instance** without asking. Old config, Host,
history and reconnect references remain unchanged. A private, atomic workspace binding makes
that choice persistent. `--instance`/`--config` remain explicit destinations.
A new session name alone cannot change an already-started Host's registration.
See the packaged Japanese README for both environments' identical commands.
The provider-specific Pilot profiles below are retained compatibility paths.

An unchanged profile reuses its original instance after the native launcher has
saved `runtime.json`, across new Bash sessions and an identical reinstall. The
wrapper compares native `managedPolicyMode` with the same Runtime → Providers →
Launcher precedence; an empty config object overrides lower-precedence values,
while an absent/null pointer does not. It neither rewrites the native config nor
treats a cached availability change as approval to change the provider route.

The packaged regression gate below uses fresh synthetic HOMEs, fake provider
authentication/catalog/generations and the archive's actual installed wrapper.
It checks native first-run saving, three normal `unreal development` openings,
reinstall, detach, stopped-Session resume and explicitly approved profile updates.
Read-only Gateway inspection observes the results; it does not replace wrapper
reconnection. Existing separate routing tests verify Codex → Claude → Codex with
canonical receipts in the same archive. No real inference is part of this gate.

```sh
UNREAL_COMMON_KIT_DIRECTORY=/absolute/path/to/extracted/common-kit \
UNREAL_COMMON_BIN_DIR=/absolute/path/to/extracted/common-kit \
go test ./cmd/unreal-agent -run '^TestCommon' -count=1 -v
```

For an existing **asp** Sandbox using Docker host-owned ChatGPT OAuth, see
[Docker Sandbox OAuth compatibility](docker-sandbox-oauth.md). Old 6284e49
binaries cannot use the new explicit `docker_sandbox_oauth` route. Build a new
snapshot from the validated OAuth implementation revision and package it with:

```sh
python3 scripts/asp_oauth_bundle.py --snapshot /absolute/path/to/snapshot.tar.gz \
  --revision <full-snapshot-revision> --copy-to /mnt/c/work/unreal-agent-pilot/asp
make test-asp-oauth
# Optional native install/reinstall/preflight/idle Host tests, no inference:
UNREAL_ASP_OAUTH_KIT=/absolute/path/to/extracted/kit make test-asp-oauth
```

The new kit contains all four CGO Linux amd64 binaries, bounded native auth
preflight, a read-only asp profile and Japanese instructions. Packaging uses
the same snapshot/checksum/ELF/libc validators and reserves fresh output and
Windows delivery directories. It never updates the devlore kit. The existing
6284e49 installer and provider profiles retain their default behavior.

The operator explicitly sources `activate.sh --model <id>` inside asp, then
uses `unreal <session>` and `/model`. Activation installs into a new private,
content-addressed directory and defines `unreal` in **that shell only**. It
does not change PATH, shell startup files, Codex auth/config, default runtime
configuration or old Session stores. The wrapper invokes native `unreal`
with a dedicated config/state/socket, no automatic prompt. `--smoke-only` is
an explicit operator opt-in to one short real request; WSL tests never send
that request to a real provider. `unset -f unreal` restores shell command
resolution. The Pilot history is retained.

Build the Fork's current worktree on **Linux amd64** with Go 1.27, a working
C compiler, Python 3.12 or newer, and **GoReleaser 2.18.2**:

```sh
make test-snapshot
make snapshot
# If GoReleaser is installed elsewhere:
make snapshot GORELEASER=/absolute/path/to/goreleaser
```

The wrapper runs `goreleaser check` separately, then
`goreleaser release --snapshot --skip=publish`. It reserves a new private
`dist/snapshots/snapshot-*/` directory on every invocation. It never uses
`--clean`, overwrites a previous snapshot, modifies runtime/auth configuration,
or sends a real model request. Failed snapshots remain available for inspection.
No GitHub token, tag, Release, Docker upload, or Homebrew tap is involved.
GoReleaser's `dist` field is not templated: the wrapper writes a private copy
of the same configuration with only that path replaced by the reserved
directory's `artifacts/` path. Build, archive, and publishing semantics are
otherwise identical to the checked-in configuration.

The configuration in `.goreleaser.yaml` is snapshot-only. Its pre-build hook
rejects non-snapshot runs, and release publishing is disabled. The existing
tag-triggered release workflow has **not** been enabled or migrated for this Fork.
Invoke snapshots through the wrapper so output directories are safely reserved.

## Executables

| Archive name | Entrypoint | Responsibility |
| --- | --- | --- |
| `unreal` | `./cmd/unreal-agent` | Normal launcher: create/attach/resume and background Host readiness |
| `unreal-agent` | `./cmd/unreal-agent` | Explicit `serve`, `attach`, and internal child protocol |
| `unreal-agent-runner` | `./cmd/unreal-agent-runner` | JSON request / persisted JSONL session runner |
| `unreal-agent-auth` | `./cmd/unreal-agent-auth` | Dedicated credential control CLI |

`unreal` and `unreal-agent` are built from the same main package under both
names; the existing `argv[0]` dispatch selects their behavior. This archive uses
two regular executables instead of requiring a symlink installer. Keep both
names when installing. The Live Dock TUI remains the Host's attach projection;
there is no separate upstream TUI binary or alias replacing the Host CLI.
Codex and Claude Code retain ownership of their external authentication.

## Artifacts and provenance

The `artifacts/` subdirectory contains a `.tar.gz` with these four executables,
`LICENSE`, this document, and `SNAPSHOT.json`, plus `SHA256SUMS` and GoReleaser's
local metadata. The wrapper verifies the checksum, exact archive inventory,
executable permissions, embedded Go/VCS metadata, and non-inference CLI smoke.
It then runs `TestSnapshotPackaged*` against the extracted archive: an idle
Host creates a Session and supports gateway attach/detach without inference;
the runner executes an AST query and returns its result to a local fake HTTP
provider. No real Claude/Codex backend is contacted.

`SNAPSHOT.json` contains the snapshot version, source revision, build time,
dirty-worktree flag, Go version, and CGO target. It contains no source-file
listing, credentials, environment dump, or canonical Session history. Binary
metadata is inspectable with `go version -m <binary>`; no new CLI flags are added.
Uncommitted and untracked Go sources participate in the build. Consequently,
the HEAD SHA alone does **not** identify this snapshot's full source state.
This is a repeatable local build procedure, not a claim of bit-for-bit
reproducibility or a reproducible formal release from the current dirty tree.

## CGO and supported target

CGO stays enabled for tree-sitter's Go, JavaScript, and Python AST parsers.
The initial target is Linux amd64 with `GOAMD64=v1`. Native GCC/Clang is required;
the wrapper refuses a non-Linux-amd64 host rather than silently disabling AST.
Linux binaries use the build machine's libc: portability to older glibc systems
is not established by this snapshot. Check dependencies with `ldd <binary>`.

Linux arm64 requires an arm64 C toolchain and execution on hardware or an
appropriate emulator. macOS amd64/arm64 require Apple SDK/toolchains (normally
native macOS CI) and native runtime/AST tests. These targets are deliberately
absent from the current matrix and are not claimed to work from WSL.

## Validation and later publication

```sh
make test-claude-structured  # fake CLI/Host; no real inference
CGO_ENABLED=1 go test -race ./...
make check
make build
git diff --check
```

Before a formal release: identify a reviewed clean source revision, validate
each target's C toolchain and runtime, establish the supported libc baseline,
add CLI version reporting if desired, and configure Fork-specific release
ownership, signing/notarization, and Homebrew policy. The inherited workflow's
upstream repository guard, CGO-disabled build matrix, and upstream Docker
destination must be reviewed separately. Publishing remains a future task.

## asp manual-install kit

The pilot kit repackages an **existing** verified Linux amd64 snapshot at
`6284e49e70fd603902133bc194ae1ca81c777edd`; it does not rebuild or connect to a
Sandbox. Supply the archive alongside its original `SHA256SUMS`:

```sh
UNREAL_PILOT_SNAPSHOT=/absolute/path/to/existing/snapshot.tar.gz make test-pilot-kit
python3 scripts/pilot_kit.py --snapshot /absolute/path/to/existing/snapshot.tar.gz \
  --copy-to /mnt/c/work/unreal-agent-pilot/asp
```

The builder requires Python 3.12, Go (build-metadata inspection only), and
`readelf` on WSL. It verifies the source checksum/inventory/provenance, four
amd64 `GOAMD64=v1` / CGO binaries, tree-sitter dependency and actual glibc
symbols. This baseline requires glibc 2.34 or newer; auth is statically linked.
Fresh private `dist/pilot-kits/kit-*` and delivery subdirectories keep earlier
files intact. The archive includes the original `SNAPSHOT.json`, `KIT.json`,
four binaries, internal `SHA256SUMS`, LICENSE, installer, doctor and Japanese
README. Its sibling `.tar.gz.sha256` verifies the whole archive.

The README has two user operations: `sbx cp` from Windows (host CLI availability
unconfirmed), then checksum/extraction/install/doctor inside asp. The installer
checks OS/architecture/glibc/libraries before writing a private version under
`$HOME/.local/opt/unreal-agent-pilot/asp/`. Identical installs are verified and
reused; changed files, symlink redirects and an active/stale install lock cause
a safe refusal. No PATH, existing Codex executable/auth/config, permissions,
default Host, OS packages or Session Store are changed. Doctor executes only
help/methods in an empty temporary HOME; auth contents and network/API access
remain unchecked. Use the original Codex command to resume normal work.

## Provider-specific Sandbox scripts

The optional operator profiles reuse that existing binary installation. They
contain only scripts, Japanese instructions and checksums; authentication,
binary/config defaults and project files are not bundled or changed:

```sh
make test-pilot-provider
# Include startup checks against the existing 6284e49 binaries (fake auth only):
UNREAL_PILOT_KIT_DIRECTORY=/absolute/path/to/verified/extracted/kit make test-pilot-provider
python3 scripts/provider_pilot_bundle.py --copy-to /mnt/c/work/unreal-agent-pilot
```

Each delivery reserves a new directory under `asp/` or `devlore/`, without
overwriting previous kits. The Japanese README gives the `sbx cp` commands
(Windows CLI availability remains unconfirmed) and a single Sandbox command.
The historical `asp/6284e49-*` binary install can also be reused in a separate
devlore home; it does not select the provider or the Session Store.

`asp` fixes the provider to `openai-codex`; preflight checks only the existing
External Codex auth file's path metadata. `devlore` fixes it to `claude-code`
with the **structured** bridge. Before any Claude invocation, devlore requires
both `--policy-audited` and `--trust-managed-policy`. The operator must first
audit that Sandbox's local/remote administrative policy, managed hooks/helpers,
environment configuration and telemetry. These flags attest review and opt in
to the existing administrative trust boundary; they are not an automatic audit
or a guarantee that remote policy remains unchanged. Missing CLI/login or
unreviewed policy stops the script. It never enables a side-effecting built-in
tool, requests MCP grants, or selects a fallback provider.

An explicit `--model` is required. Both profiles use the existing `~/workspace`,
only read/grep/glob permissions, and private per-Sandbox/per-instance config,
state and socket paths. `Launcher.DiscoverProviders=false` prevents additional
registration while retaining the existing isolated provider transport. It does
not grant tool subprocess, write or unrestricted network/filesystem access.
An existing config is reused only when identical; choose a new `--instance`
for changed settings rather than overwriting canonical startup/history.

`--preflight-only` does not start a Host or inference. A normal run verifies or
starts the dedicated Host, submits one short text-only smoke in a fresh Session,
checks the durable response and absence of Operations, then attaches the native
TUI. `--smoke-only` omits the TUI; `--attach-only` reconnects to the last smoke
Session without resubmission. Provider errors/timeouts are not retried, and
history remains available. Existing incompatible/stale sockets and Hosts are
not unlinked, killed or silently reused. Stop only the identified Pilot Host to
return to the original Codex/Claude CLI; keep its state and existing credentials.

Unit/native Host tests use dummy auth status and fake Claude only. WSL validation
does not establish Sandbox login, API connectivity, model availability, managed
policy approval or real inference success; those remain operator-run gates.

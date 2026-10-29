# Distribution and GitHub releases

## CI and pre-releases

GitHub Actions runs CI for pushes to `main` and pull requests on Ubuntu 24.04:
`make test check build` plus `cd scripts && python3 -m unittest discover -p 'test_*.py'`.
The Go/Python job has a 15-minute timeout (the latest Ubuntu job took 5m08s).
The six fuzz targets and harbor checks remain separate jobs. Distribution
supports Linux amd64 only. Tests requiring subscription logins keep their
existing environment-variable gates and are skipped when those are absent.

Pushing a `v*` tag in `TomyFovos/unreal-agent` runs the release workflow. It
checks `make test check` and the Python tests, installs GoReleaser **2.18.2**,
then runs `make snapshot` and `scripts/common_development_bundle.py` on the
tagged commit. The resulting GitHub Release is a **pre-release** with:

- `unreal-agent-linux-amd64-<rev>.tar.gz`
- `unreal-agent-linux-amd64-<rev>.tar.gz.sha256`

`<rev>` is the first seven characters of the tagged commit SHA. The archive
uses the same builder and contents as the previous local common development
kits. Download and install it using [install.md](install.md). The build job
has read-only repository access; only the separate Release creation job has
`contents: write`, using the automatic `GITHUB_TOKEN`. There is no runner-only
archive or Docker Hub publication.

Normal CI, kit builds and releases run on GitHub. Local work before Sandbox
deployment is limited to installing the downloaded kit in an isolated WSL
HOME, starting it and checking real inference with subscription login. CI's
synthetic checks and skipped login-dependent tests do not establish that
real inference works. The local build instructions below are for reproducing
or validating changes to the distribution workflow.

## Local snapshot reproduction

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

Build the Fork's current worktree on **Linux amd64** with Go 1.27, a working
C compiler, Python 3.12 or newer, and **GoReleaser 2.18.2**:

```sh
make test-snapshot
tools_dir=$(mktemp -d /tmp/unreal-release-tools.XXXXXXXX)
GOBIN="$tools_dir" go install -ldflags='-X main.version=2.18.2' github.com/goreleaser/goreleaser/v2@v2.18.2
"$tools_dir/goreleaser" --version
make snapshot GORELEASER="$tools_dir/goreleaser"
python3 scripts/common_development_bundle.py --snapshot /path/printed/by/snapshot.tar.gz \
  --revision "$(git rev-parse HEAD)"
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

The configuration in `.goreleaser.yaml` remains snapshot-only. Its pre-build
hook rejects non-snapshot runs, and GoReleaser publishing is disabled. The
tag-triggered workflow packages that verified snapshot into the common kit,
verifies its sibling checksum, transfers both files to the publishing job,
and creates the GitHub pre-release with `gh release create --prerelease`.
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

## Validation

```sh
make test check build
cd scripts && python3 -m unittest discover -p 'test_*.py'
```

The release workflow builds only Linux amd64 with CGO enabled on Ubuntu 24.04.
The kit records the binaries' minimum glibc version; the installer checks it
on the destination. Validate installation, startup and real inference in WSL
before Sandbox deployment. Other architectures and operating systems require
their own C toolchains and runtime validation before distribution support.

## asp manual-install kit

The pilot kit repackages an **existing** verified Linux amd64 snapshot at
`6284e49e70fd603902133bc194ae1ca81c777edd`; it does not rebuild or connect to a
Sandbox. Supply the archive alongside its original `SHA256SUMS`:

```sh
UNREAL_PILOT_SNAPSHOT=/absolute/path/to/existing/snapshot.tar.gz make test-pilot-kit
python3 scripts/pilot_kit.py --snapshot /absolute/path/to/existing/snapshot.tar.gz \
  --copy-to /path/to/output/asp
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

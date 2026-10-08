# Local snapshot distribution

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

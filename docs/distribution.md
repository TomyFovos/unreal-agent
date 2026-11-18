# Distribution

**Language:** English | [日本語](ja/distribution.md)

The official distribution format is the **Linux amd64 common kit** (`CGO_ENABLED=1` with Tree-sitter AST support). A single kit supports both Claude and Codex while keeping authentication owned by the external CLIs.

---

## 1. Local Checks and Continuous Integration

Before committing any change, run the pre-commit check script:

```bash
make prepush
# Or check all changes since a specific revision:
make prepush BASE=<rev>
```

### What Runs Where

| Check Suite | Command / Workflow | What It Verifies |
| --- | --- | --- |
| **Local pre-commit** | `make prepush` | `gofmt`, `go vet`, affected Go package tests (or `./...` when non-Go files change), Python script tests (`scripts/test_*.py`), and Harbor checks if `benchmarks/harbor/` changed |
| **Main CI** | [`.github/workflows/ci.yml`](../.github/workflows/ci.yml) | Full `make test check build` with `-race` on Ubuntu 24.04, Python script tests, Harbor adapter build & Docker smoke test |
| **Fuzzing** | [`.github/workflows/fuzz.yml`](../.github/workflows/fuzz.yml) | Scheduled/manual Go fuzzing (normal `go test` runs always execute seed corpora) |

---

## 2. Building a Snapshot and Common Kit Locally

### Prerequisites (Linux amd64)

- Go version specified in [`go.mod`](../go.mod)
- GCC / C compiler (`CGO_ENABLED=1`)
- Python 3 (CI uses 3.12)
- GoReleaser `v2.18.2`

### Build Steps

1. **Run snapshot script tests and install pinned GoReleaser:**
   ```bash
   make test-snapshot
   unreal_release_tools=$(mktemp -d /tmp/unreal-release-tools.XXXXXXXX)
   GOBIN="$unreal_release_tools" go install -ldflags='-X main.version=2.18.2' github.com/goreleaser/goreleaser/v2@v2.18.2
   "$unreal_release_tools/goreleaser" --version
   ```

2. **Build and verify a snapshot archive:**
   ```bash
   make snapshot GORELEASER="$unreal_release_tools/goreleaser"
   ```
   [`scripts/snapshot.py`](../scripts/snapshot.py) creates a fresh directory under `dist/snapshots/snapshot-*/`, runs GoReleaser in snapshot-only mode ([`.goreleaser.yaml`](../.goreleaser.yaml) blocks direct publishing), writes `SNAPSHOT.json`, and verifies binary permissions, checksums, CGO/VCS metadata, `--help` flags, and local Host attach/detach behavior.

3. **Assemble the redistributable common kit:**
   Pass the verified snapshot `.tar.gz` path and full 40-character commit hash from `SNAPSHOT.json`:
   ```bash
   read -r -e -p 'Verified snapshot archive: ' unreal_snapshot
   read -r -p 'Full source revision: ' unreal_revision
   python3 scripts/common_development_bundle.py --snapshot "$unreal_snapshot" --revision "$unreal_revision"
   ```

> **Notes:**
> - The snapshot is built from the current worktree, including uncommitted changes (`SNAPSHOT.json` records whether the worktree was dirty).
> - Packaging smoke checks use synthetic data and do not perform real model inference.

### Output Files

[`scripts/common_development_bundle.py`](../scripts/common_development_bundle.py) produces:
- `unreal-agent-linux-amd64-REV.tar.gz`
- `unreal-agent-linux-amd64-REV.tar.gz.sha256`

(`REV` is the 7-character short commit hash. Inside the archive are the 4 binaries, `KIT.json`, `SNAPSHOT.json`, `SHA256SUMS`, `README.md`, `LICENSE`, `install.sh`, `doctor.sh`, `development.sh`, and Python helper scripts.)

---

## 3. Packaged Executables and Upgrade Behavior

| Binary | Role |
| --- | --- |
| `unreal` | Native create/attach/resume launcher (wrapped by `~/.local/bin/unreal` when installed via the common kit) |
| `unreal-agent` | Background Host daemon (`serve`), TUI client (`attach`), and sub-agent CLI (`subagent`) |
| `unreal-agent-runner` | Headless single-prompt / JSONL evaluation runner |
| `unreal-agent-auth` | Managed credential store CLI for developer/evaluation setups |

When a user installs a newer common kit and runs `unreal <name>` in an existing workspace:
- The wrapper detects the updated build or provider profile, restarts that workspace's background Host automatically, and **retains all existing Sessions and settings**.
- The installer keeps the current version, the immediately previous version, and any version in use by a live process, and prunes older installed versions.

---

## 4. GitHub Release Workflow

The [Release workflow](../.github/workflows/release.yml) triggers on `v*` tags pushed to `TomyFovos/unreal-agent`:

1. **Verification:** Confirms the tagged commit is an ancestor of `main` and waits for the exact commit's CI workflow to pass.
2. **Read-only Build Job:** Installs GoReleaser `v2.18.2`, builds the snapshot and common kit, and verifies checksums without write permissions.
3. **Publish Job:** Uses `contents: write` to create the GitHub Release and upload `unreal-agent-linux-amd64-REV.tar.gz` and `.sha256` (tags containing `-` are marked as prereleases).

> **Governance Rule:** Pushing commits, opening/merging PRs, pushing tags, and publishing Releases always require explicit prior approval from the user for that specific commit.

---

## 5. Pre-Distribution Acceptance Checklist

Before handing out a kit:

1. **Verify CI & local tests:** Confirm `make prepush` and CI pass on the exact commit (distinguish skipped login-dependent tests from actual passes).
2. **Real install & live inference test:** Install the kit into a separate test destination on WSL, launch the TUI, and complete at least one real subscription model turn without altering user credentials or production sessions.
3. **Test modified flows:** If you changed session lifecycle, permissions, or file tools, test detach/reconnect/resume, permission retention, and actual file contents on disk.
4. **Report verification details:** Record the tested kit filename, commit revision, environment, real inference result, and any unverified paths before sharing the kit with [Installation instructions](install.md).

> **Important:** A successful build, `doctor.sh`, or packaging smoke test alone does not satisfy live inference acceptance.

## Distribution builds without OpenCode Go

OpenCode Go is enabled by default. A distributor can compile the entire connection out of use with the Go build tag `disable_opencode_go`. The resulting executables use a compiled constant; installed configuration files, saved keys, and environment variables cannot enable it again. Rebuild from source to change the choice. Other providers keep their existing behavior.

Build a disabled snapshot and common kit:

```bash
make snapshot OPENCODE_GO_ENABLED=false GORELEASER=/path/to/goreleaser | tee /tmp/unreal-disabled-snapshot.log
unreal_snapshot=$(sed -n 's/^snapshot verified: //p' /tmp/unreal-disabled-snapshot.log)
python3 scripts/common_development_bundle.py --snapshot "$unreal_snapshot" \
  --revision "$(git rev-parse HEAD)" --disable-opencode-go
```

The Python build entry point is `python3 scripts/snapshot.py build --disable-opencode-go`. `.goreleaser.yaml` applies the tag to all four binaries when its build environment has `OPENCODE_GO_ENABLED=false`; the snapshot wrapper supplies it. Common, pilot, and permission kit packagers accept `--disable-opencode-go` to require a disabled input snapshot. They preserve its choice and reject an enabled input rather than relabeling its binaries.

Other source builds:

```bash
make build OPENCODE_GO_ENABLED=false
make -C benchmarks/harbor build REVISION=HEAD OPENCODE_GO_ENABLED=false BUNDLE=/absolute/new-harbor-bundle
# Equivalent Harbor builder:
python3 benchmarks/harbor/build.py --revision HEAD --output /absolute/another-new-bundle --disable-opencode-go
make mac-install REV=HEAD OPENCODE_GO_ENABLED=false
# Or: sh scripts/mac/install.sh HEAD --disable-opencode-go
# Docker build:
docker build --build-arg OPENCODE_GO_ENABLED=false -t unreal-agent-local .
# Direct Go builds (apply to every executable being distributed):
go build -tags=disable_opencode_go ./cmd/unreal-agent
```

Mac installs keep enabled and disabled variants of the same commit in separate directories. The Release workflow uses the distributor's repository variable `UNREAL_OPENCODE_GO_ENABLED=false` at build time. This is independent of end-user environment variables.

`SNAPSHOT.json`, `KIT.json`, and Harbor's `manifest.json` record the Boolean `opencode_go_enabled`. All product executables report `opencode_go_enabled=false` with `--version`. Disabled builds omit OpenCode Go from discovery, catalogs, `/model`, and authentication methods; `unreal-agent subagent model set --provider opencode-go` is rejected. The installer refuses its key file and key environment-variable options without printing the key. A Session saved with OpenCode Go remains inspectable with its original selection and a notice directing the user to choose another connection with `/model`; it sends no inference request and never automatically changes providers. A child requesting OpenCode Go fails with guidance to select another connection with `unreal-agent subagent model`.

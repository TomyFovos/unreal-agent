# Sandbox daily development bootstrap

After installing the daily kit, open a new Bash session:

```sh
cd ~/workspace/my-project
unreal --workspace-info development
unreal development
```

The user chooses a real Git repository. `~/workspace` need not itself be a repo.
The wrapper resolves its physical Git root using bounded metadata-only Git,
changes cwd to that root, and passes a repo-specific explicit configuration,
state and socket to the **existing native launcher**. It does not submit a
message, create a second lifecycle implementation, or run any tool automatically.
Native launcher locks/readiness/Host identity checks still own create/attach/resume.

asp uses the existing explicit Docker Sandbox OAuth preflight, exact routing
and HTTPS forward-proxy contract. devlore requires an authenticated supported
Claude CLI and explicit `--policy-audited --trust-managed-policy`. It uses the
existing Structured Bridge. No auth contents are imported by the wrapper, no
secrets are packaged, and there is no provider/API-key/MCP fallback.

The persistent namespace is `unreal-agent-dev/<sandbox>` in the user's config
and local state directories, separate from default Host and all Gate D/E paths.
Repo keys use 16 hex SHA-256 characters of the canonical path, with the complete
hash and path checked in a private binding before reuse. Clones and separate
Git worktrees are isolated; equal Session names in distinct roots cannot attach
to each other's Host. Symlink aliases and subdirectories reuse the same root.
Missing repos, bare repos, inherited Git workspace overrides, unsafe path
characters and repositories overlapping private storage fail before Host startup.
Paths too long for the native Unix socket bound fail without a shared fallback.

Fresh configurations reuse the existing Read Only base and human-approved
PermissionModes ceiling. Ask/Full Access require the normal authenticated TUI
control path; installing the kit grants neither mode. Ambient Full Access is
not OS isolation or Git publication approval. Native file tools keep their
workspace boundary, explicit denies and child ceilings remain effective, and
Host restart expires elevation. Existing repo configuration is never regenerated
from changed initial model/effort defaults. Existing Session runtime selection,
history, Operations and receipts remain native-owned.

`scripts/daily_development_bundle.py` shares the existing snapshot/kit assembler,
CGO/glibc diagnostics and content-addressed binary installer. `daily.sh install`
publishes `~/.local/bin/unreal` and reuses guarded Bash PATH setup. Identical
installation leaves config/command/profile contents and mtimes intact. A changed
verified owned wrapper requires `--replace-pilot-launcher`; only that exact
wrapper or the checksummed legacy devlore launcher-v1 can be replaced. Old bytes
are backed up privately. Arbitrary commands, aliases, functions and symlinks
are preserved and cause a conflict. Kit generations, old Gate stores and auth
files are never removed. Installation cannot initiate model inference.

Quick Start reuses the existing explicit Enter/no-timeout confirmation. Full
instructions are available through `unreal --readme`. `unreal --workspace-info`
shows the selected root and persistent paths without starting a Host. It may
bootstrap the private configuration for a fresh repo. Detach and reconnect use
the same `unreal SESSION_NAME`; existing TUI scroll, selection, TPS, model and
permission controls are unchanged. Existing Hosts are not killed during updates.

Test with `make test-daily-development`; optionally set
`UNREAL_DAILY_KIT_DIRECTORY` and `UNREAL_DAILY_FAKE_CLAUDE` to exercise a freshly
packaged kit with only a local fake provider and real PTY. No test automatically
connects to a Sandbox or sends a real model request. See the generated Japanese
README for transfer, installation, chosen-repo smoke and Codex CLI recovery.

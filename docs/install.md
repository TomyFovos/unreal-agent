# Installation

**Language:** English | [日本語](ja/install.md)

The Unreal Agent common kit runs on **Linux amd64** (including WSL and Linux Docker Sandboxes). One installation supports Claude and ChatGPT subscription logins and optional OpenAI / Anthropic API key connections.

## System Requirements

- **OS / Architecture:** Linux amd64 (glibc version is checked automatically by `doctor.sh` against `KIT.json`)
- **Required tools:** `bash`, `python3`, `git`, `tar`, `sha256sum`
- **Provider access:** Subscription login or an explicitly selected API key. Claude requires the official `claude` CLI for either authentication method. OpenAI API needs no Codex subscription login. ChatGPT uses `codex login`; Codex inference can run without its CLI while valid authentication and a model catalog are available, including host-managed Docker Sandbox OAuth.

---

## Step 1: Sign In to a Provider

For a subscription connection, prepare the login inside the Linux environment where Unreal will run. To use an API key instead, follow the optional registration below; a subscription login is unnecessary:

- **Claude:** Install the official Claude Code CLI, run `claude` in Bash, and execute `/login` inside Claude Code with your Claude subscription.
- **Codex:** Install the official Codex CLI and run `codex login` in Bash with your ChatGPT subscription.
- **Docker Sandbox (host-managed Codex OAuth):** Prepare the host login instead. See [Providers — Docker Sandboxes Codex OAuth](providers.md#docker-sandboxes-codex-oauth).

> **Note:** Running `/login` inside Unreal displays subscription sign-in instructions. With neither subscription ready, installation still succeeds (exit code `0`). After a later login, run `/model refresh` and select the connection; no reinstall is needed. If you explicitly disabled a subscription provider, re-enable it with `bash development.sh --configure-providers --codex-auto` for Codex or `bash development.sh --configure-providers` for Claude.

### Optional: Register an API Key Connection

Prepare key files with **0600** permissions and add either or both flags to the installation command:

```bash
chmod 600 /absolute/openai.key /absolute/anthropic.key
bash development.sh --openai-api-key-file /absolute/openai.key --anthropic-api-key-file /absolute/anthropic.key
```

Settings store only the file paths, never key values. Files without permissions 0600 are rejected. Alternatively, explicitly name an environment source with `--openai-api-key-env NAME` or `--anthropic-api-key-env NAME`.

**API billing is separate from the subscription.** Merely having `OPENAI_API_KEY` or `ANTHROPIC_API_KEY` in the environment never enables API use. Registration does not select an API connection: after launch, choose `OpenAI API（API キー・別課金）` or `Claude（API キー・別課金）` in `/model`. This also applies when only API connections are registered. Sessions keep their selection, and expired subscriptions never fall back to API keys. See [Providers](providers.md#optional-api-key-connections) for explicit initial selection and nonsecret `runtime.json` references.

---

## Step 2: Download and Install the Kit

1. Download both `unreal-agent-linux-amd64-REV.tar.gz` and `unreal-agent-linux-amd64-REV.tar.gz.sha256` from [GitHub Releases](https://github.com/TomyFovos/unreal-agent/releases) into the same directory (`REV` is the 7-character commit hash).
2. Run the following block in Bash and enter the full path to the `.tar.gz` file when prompted:

   ```bash
   set -e
   read -r -e -p 'Full path to the kit .tar.gz: ' unreal_archive
   unreal_archive=$(realpath -- "$unreal_archive")
   cd -- "$(dirname -- "$unreal_archive")"
   sha256sum -c "$(basename -- "$unreal_archive").sha256"
   unreal_unpack=$(mktemp -d /tmp/unreal-common.XXXXXXXX)
   tar -xzf "$unreal_archive" -C "$unreal_unpack"
   cd -- "$unreal_unpack/$(basename -- "$unreal_archive" .tar.gz)"
   bash doctor.sh
   bash development.sh --configure-providers --replace-pilot-launcher
   . "$HOME/.config/unreal-agent-common/path.sh"
   ```

3. Check the printed connection summary.
   Among ready subscriptions, a new installation starts with Claude when both are ready, or the ready subscription. API connections require explicit selection in `/model`. Claude uses GPT children when Codex is available, otherwise Claude Code's Agent (`haiku` / `sonnet`, or the Claude model chosen with `/model-sub`). Without a saved `/model-sub` choice, an OpenAI API parent uses its API connection for built-in GPT children; other parents retain Codex. An explicit `/model-sub` choice takes priority. Unavailable authentication never triggers a connection switch.
4. Navigate to your project directory and start a named Session:

   ```bash
   unreal development
   ```

   Enter `y` when asked to create the Session.

### What Gets Installed

- **Commands:** `~/.local/bin/unreal` and `~/.local/bin/unreal-agent`
- **Shell helper:** `~/.config/unreal-agent-common/path.sh` (sourced automatically in new Bash shells; adds `~/.local/bin` to `PATH` and defines the `ucd` helper function)
- **Workspace root:** Automatically set to the current Git worktree root, or the current directory if outside Git.

---

## Mac: `unreal NAME` from any folder

On macOS, install a revision from this Git checkout (`REV` is required; it can be a full or abbreviated commit hash, or another revision Git resolves to a commit such as a tag or branch):

```bash
make mac-install REV=<commit>
```

The build runs in a detached worktree and installs to `~/.local/opt/unreal-agent/<rev>`; the `current` link is switched to it and `~/.local/bin/unreal` is written. The current version, the version it replaced and any version a running process still uses are kept; other versions are removed.

If `~/.local/bin` is not in `PATH`, add the printed line to `~/.zshrc` and reopen the shell:

```bash
export PATH="$HOME/.local/bin:$PATH"
```

Then, from any folder:

```bash
unreal NAME             # open Session NAME in this folder, creating it if no folder has it
unreal                  # list every Session in every folder; pick a number to open one
unreal --where NAME     # print the folder(s) that have Session NAME
```

(`unreal-agent folders ...` is the same subcommand without the wrapper.)

The folder is the Git worktree root containing the current directory (the directory itself outside Git), with symlinks resolved. One Host runs per folder, with its own configuration `~/.config/unreal-agent/folders/<ID>/runtime.json` and state `~/.local/state/unreal-agent/folders/<ID>/` (`<ID>` is `r-` plus the first 16 hex characters of the SHA-256 of the folder path). The default folder — `Runtime.Workspace` in `~/.config/unreal-agent/runtime.json` — keeps that file and `~/.local/state/unreal-agent` and launches with no extra flags. The Claude login, the OpenCode Go API key and the `/model-sub` choice are shared by every folder; the Claude sandbox allows only that Host's socket, so the subagent CLI keeps working.

Roll back with an older revision: `make mac-install REV=<older commit>`.

Installing or rolling back does not change a Host that is already running: `unreal NAME` attaches to it, and it keeps the version it started with. To switch, detach every Session (`Ctrl+D`), stop the Hosts with `pkill -f 'unreal-agent serve'`, then run `unreal NAME`; the Host starts again from the installed version. Conversations and settings are kept; children still running are canceled when their Host stops.

---

## Installing into a Docker Sandbox (from Windows)

If you use an existing Linux Docker Sandbox on Windows:

1. **Transfer the archive from Windows PowerShell:**
   Keep the `.tar.gz` and `.tar.gz.sha256` files in the same folder, then run:

   ```powershell
   $unrealArchive = Get-Item -LiteralPath (Read-Host 'Full path to the kit .tar.gz')
   $unrealSandbox = Read-Host 'Existing Sandbox name'
   sbx cp $unrealArchive.FullName "${unrealSandbox}:/tmp/$($unrealArchive.Name)"
   if ($LASTEXITCODE -ne 0) { throw 'Archive transfer failed' }
   sbx cp "$($unrealArchive.FullName).sha256" "${unrealSandbox}:/tmp/$($unrealArchive.Name).sha256"
   if ($LASTEXITCODE -ne 0) { throw 'Checksum transfer failed' }
   sbx exec -it $unrealSandbox bash
   ```

2. **Install inside Sandbox Bash:**

   ```bash
   set -e
   cd /tmp
   read -r -p 'Transferred kit filename (.tar.gz): ' unreal_archive
   sha256sum -c "$unreal_archive.sha256"
   unreal_unpack=$(mktemp -d /tmp/unreal-common.XXXXXXXX)
   tar -xzf "$unreal_archive" -C "$unreal_unpack"
   cd -- "$unreal_unpack/${unreal_archive%.tar.gz}"
   bash doctor.sh
   bash development.sh --configure-providers --replace-pilot-launcher
   . "$HOME/.config/unreal-agent-common/path.sh"
   ```

3. Change to your project directory inside the Sandbox and run `unreal development`.

> **Reference:** Uses Docker's [`sbx cp`](https://docs.docker.com/reference/cli/sbx/cp/) and [`sbx exec`](https://docs.docker.com/reference/cli/sbx/exec/) commands against an existing Sandbox.

---

## Updating an Existing Installation

1. Download the new `.tar.gz` archive and its `.sha256` checksum file.
2. Run the same installation block from [Step 2](#step-2-download-and-install-the-kit).
3. Reopen your Session as usual (`unreal development`).

When you launch a Session after updating, the wrapper detects the new build, restarts that workspace's background Host automatically, and **preserves your existing conversation history and settings**. Old versions are cleaned up automatically while keeping the current version, previous version, and any version attached to a live process.

### Optional Installer Flags (`development.sh`)

Pass these flags to `bash development.sh` during installation or update (not to the `unreal` command):

| Flag | Description |
| --- | --- |
| `--codex-auto` | Automatically detect external Codex login or Docker Sandbox OAuth (default on fresh install) |
| `--external-codex` | Select external Codex login (`OPENAI_CODEX_AUTH_FILE`, then `$CODEX_HOME/auth.json`, then `~/.codex/auth.json`) |
| `--docker-sandbox-oauth` | Require Docker Sandbox host-managed OAuth proxy routing |
| `--disable-codex` | Disable the Codex provider |
| `--disable-claude` | Disable the Claude provider |
| `--claude-binary <path>` | Specify a custom path to the `claude` executable |
| `--openai-api-key-file <path>` | Register an optional OpenAI API connection using a 0600 key file; separate billing |
| `--anthropic-api-key-file <path>` | Register an optional Claude API connection using a 0600 key file; separate billing |
| `--openai-api-key-env <NAME>` | Explicitly choose the OpenAI key's environment variable name |
| `--anthropic-api-key-env <NAME>` | Explicitly choose the Anthropic key's environment variable name |

### OpenCode Go key and distributor build choice

Enabled kits also accept `bash development.sh --opencode-go-api-key-file /absolute/go.key` (a 0600 file) or `--opencode-go-api-key-env MY_GO_KEY` (an explicitly named environment variable). The key is stored in Unreal’s managed store without printing it; select OpenCode Go explicitly with `/model`.

A kit marked `opencode_go_enabled: false` in `KIT.json` rejects both options with a distribution-disabled message. Saving a key through `unreal-agent-auth` also fails in that build. Runtime settings and environment variables cannot restore the connection. An older Session choosing it stays open with guidance to use `/model` for another connection. Use `unreal-agent --version` to check the compiled choice; rebuilding from source is required to change it. See [distribution builds](distribution.md#distribution-builds-without-opencode-go).

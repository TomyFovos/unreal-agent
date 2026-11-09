# Installation

**Language:** English | [日本語](ja/install.md)

The Unreal Agent common kit runs on **Linux amd64** (including WSL and Linux Docker Sandboxes) and supports **Claude only, ChatGPT (Codex) only, or both** in a single installation.

## System Requirements

- **OS / Architecture:** Linux amd64 (glibc version is checked automatically by `doctor.sh` against `KIT.json`)
- **Required tools:** `bash`, `python3`, `git`, `tar`, `sha256sum`
- **Provider login:** The official `claude` CLI for Claude, or `codex login` for ChatGPT. Codex inference can run without its CLI while valid authentication and a model catalog are available, including host-managed Docker Sandbox OAuth.

---

## Step 1: Sign In to a Provider

Prepare a subscription login for whichever provider you want to use inside the Linux environment where Unreal will run:

- **Claude:** Install the official Claude Code CLI, run `claude` in Bash, and execute `/login` inside Claude Code with your Claude subscription.
- **Codex:** Install the official Codex CLI and run `codex login` in Bash with your ChatGPT subscription.
- **Docker Sandbox (host-managed Codex OAuth):** Prepare the host login instead. See [Providers — Docker Sandboxes Codex OAuth](providers.md#docker-sandboxes-codex-oauth).

> **Note:** Unreal does not accept API keys in normal interactive use. Running `/login` inside Unreal displays these sign-in instructions. With neither provider ready, installation still succeeds (exit code `0`), and Unreal starts and shows login guidance on the first input. After a later login, run `/model refresh` and select the provider in `/model`; no reinstall is needed. If you explicitly disabled a provider, re-enable it with `bash development.sh --configure-providers --codex-auto` for Codex or `bash development.sh --configure-providers` for Claude.

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

3. Check the printed availability summary for Claude and Codex.
   With both ready, a new installation starts with Claude; with one ready, it starts with that provider. Claude uses GPT children when Codex is available, otherwise Claude Code's Agent (`haiku` / `sonnet`). A GPT parent always uses GPT children and reports unavailable Codex with login guidance.
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

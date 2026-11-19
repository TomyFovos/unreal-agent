# Troubleshooting

**Language:** English | [日本語](ja/troubleshooting.md)

## Login Required or Expired

If you see `auth required`, `external_reauth_required`, `OAuth session expired`, or `OAuth token has expired`, the active provider's external login needs to be refreshed. Unreal never switches providers automatically or falls back to an API key.

### How to Fix

1. **Refresh the login in the same Linux environment where Unreal runs:**
   - **Claude:** Open a Bash shell, run `claude`, and execute `/login` inside Claude Code. *(In a Docker Sandbox on Windows, you can run `sbx exec -it <sandbox> claude`.)*
   - **Codex (Standard):** Run `codex login` in Bash.
   - **Codex (Docker Sandbox OAuth):** Repair the host OAuth login (`sbx secret set openai --oauth`) and Sandbox proxy route. See [Providers](providers.md#docker-sandboxes-codex-oauth).
2. **Reload the provider in Unreal:** Return to Unreal and run `/model refresh` (or select the provider in `/model`).
3. **Resume if stopped:** If the Session stopped due to the error, run `/resume`.

> **Note:** Signing in on your Windows/macOS host does not automatically sign in a Linux WSL environment or Docker Sandbox unless you are using configured Docker Sandbox OAuth forwarding. Also note that `/login` inside Unreal only displays sign-in instructions; actual authentication always happens in `claude` or `codex`.

---

## Startup or Command Lookup Fails

If `unreal` does not start or points to the wrong command:

1. **Check command resolution and workspace paths in Bash:**
   ```bash
   type -a unreal claude codex
   "$HOME/.local/bin/unreal" --workspace-info
   ```
2. **Fix `PATH` or command shadowing:**
   If another alias or binary shadows `unreal`, either invoke `"$HOME/.local/bin/unreal"` directly or reload the shell setup:
   ```bash
   . "$HOME/.config/unreal-agent-common/path.sh"
   ```
3. **Create a named Session first:**
   Running bare `unreal` only lists existing Sessions. To create your first Session in a project, specify a name:
   ```bash
   unreal development
   ```
4. **Verify binaries and system libraries:**
   From the extracted kit directory, run `bash doctor.sh` to verify binary checksums and glibc compatibility.

> **Tip:** `unreal --workspace-info` inspects the **current directory's** workspace. To inspect a Session located in another project, run `ucd <session-name>` first, then run `unreal --workspace-info`. Do not delete or overwrite config/session files manually to clear a workspace mismatch error.

---

## Models Are Missing or Out of Date

1. Confirm that the external CLI (`claude` or `codex`) is installed and signed in.
2. **For standard Codex OAuth:** If `models_cache.json` is missing on a fresh install, start the `codex` CLI once so it populates its local model cache.
3. Run `/model refresh` inside Unreal to re-probe provider availability and model catalogs.

> **Notes:**
> - In Docker Sandbox Codex OAuth mode, if a live `codex app-server` catalog query fails or times out, Unreal retains the last successful cached catalog (`~/.cache/unreal-agent/codex-cli-catalog.json`).
> - A model appearing in the catalog does not guarantee that your subscription tier has entitlement to run it.

---

## Reconnecting, Resuming, and Retrying

1. **Reconnect after `Ctrl+D` or disconnect:** Run `unreal <name>` (e.g., `unreal development`). The TUI rebuilds the view from canonical history.
2. **Resume a stopped Session:** Run `/resume` inside the TUI (or reopen with `unreal <name>`). If the previous provider is unavailable, switch providers with `/model` first, then run `/resume`.
3. **Resend an uncertain message (`/retry`):** If a network or socket hiccup left a submission in an uncertain state, run `/retry`. It reuses the exact submission ID so your prompt is never duplicated.
4. **Session locked by another process (`writer_owned`):** Close the other terminal/process that owns the Session and reconnect. Never delete lock files or live Session files manually.

If a parent's model turn fails, running children keep working and can still report to the parent. Send a new message to continue the parent turn. On Host restart, the parent reconnects to children whose executors still hold their writer leases and receives one end notice for each abandoned child.

---

## Finding Logs and State Files

Run `unreal --workspace-info` inside your project to print Workspace, Config, State, and Socket. The log path is not printed; `host.log` is inside the displayed State directory.

For the common kit, files are stored under:
- **Workspace configs:** `~/.config/unreal-agent-common/repos/`
- **Workspace state & logs:** `~/.local/state/unreal-agent-common/r-…/` (workspace-specific; contains `host.log` and `sessions/`)
- **Launcher settings:** `~/.config/unreal-agent-common/launcher.json`

Check `host.log` around the timestamp of any failure.

> **Warning:** Files inside `sessions/` are your canonical conversation records, not temporary debug logs—do not delete them. When reporting bugs, never share raw credential files or private session transcripts.

---

## Read analysis and measurements

Run `/analyze` in the TUI to inspect detailed Session diagnostics and token accounting.

1. Run `/analyze` to open the view picker.
2. Select a view (`Enter` or `Esc` returns to the picker).
3. Select **Export** to save the analysis report as JSON or Markdown (or use `/export all` for the full conversation with statistics).

### `/analyze` Views

| View | What It Shows |
| --- | --- |
| **Overview** | Active provider/model, response & tool counts, elapsed time, and Host commit/build metadata |
| **Usage** | Input tokens, cached input tokens, reasoning/output tokens, and Claude turn-total observations |
| **Turns** | Per-turn provider, model, effort, and token usage |
| **Tools** | Tool operation outcomes and Claude Bash hook durations (`native_bash`) |
| **Agents** | Child agents, final statuses, and `child_start` template/model/effort records |
| **Context** | Context window size, average input tokens and historical compaction observations |
| **Timeline** | Chronological event log and activity durations |
| **Errors** | Recorded errors and redacted provider diagnostics |
| **Layers** | Request and worker-model comparisons across the Session tree |
| **Context metrics** | Three-layer windows, excerpt fits, historical summary records, per-window cache and L1/L2/L3 measurements, child reviews, and user questions per work hour |
| **Export** | Save the analysis views, including the context measurement tables, as JSON or Markdown |

**Context metrics** includes archived descendants and labels values `measured`, `derived`, or `unknown`. [Three-layer context measurements](context-3layer.md#measuring)

### How Metrics Are Calculated

- **Cache percentage:** `cached_input / input` across reported usage.
- **Footer context vs. Average input:** The TUI footer shows the **latest request's** context size (not the sum of a multi-step tool loop). Average input in `/analyze` averages across all reported model calls (including calls recorded under the historical summary strategy).
- **Generation speed (`tok/s`):** Calculated from reported output tokens and generation time. A `~` prefix indicates client-clock timing. Claude Bash elapsed time measures from the pre-tool hook to completion.
- **Claude turn-total usage:** Already included in response totals—do not add it a second time.
- **Unknown vs. Zero:** If a provider does not report token usage, Unreal displays it as **unknown** (`—`), never as `0`. Labels like `partial/incomplete history` or `resync` indicate that statistics reflect only the observed portion of history.

### Context budget errors

`required_context_exceeds_budget` reports the input budget and the sizes of the system and tool header, the L2 state copy, and the required L1 tail before and after attempting to fit it. Use the breakdown to identify whether the active request, tool output, or the state copy accounts for the size. If a provider rejects an already admitted request for its context size, Unreal rebuilds the window and retries once. [Three-layer window settings](context-3layer.md#rebuild-the-responses-request-window)

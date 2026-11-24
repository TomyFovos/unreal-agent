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

Run `/analyze` to open the full-screen view list. Use ↑/↓ and **Enter** to open a view. Scroll with ↑/↓ or **PgUp/PgDn**. **Esc** or **Enter** returns from a view to the list. **Esc** from the list returns to chat. The list opens while history loads.

| View | Question | How to read it |
| --- | --- | --- |
| **Overview** | Is this Session healthy right now? | Check the running or wait state, compare the last hour's cache read share with the Session, then check token use, remaining allowances, recent failures and active children by model. |
| **Usage** | What consumed tokens, and where is waste? | Models and roles are ranked by weighted tokens and show their share of the Session. Stacks split cache reads, writes, uncached input and output. Compare writes on the first request after a rebuild with other requests. Scroll for the five highest-usage children and each model's hourly chart. |
| **Subscription usage** | When will each allowance run out? | Compare each remaining gauge with its reset. The chart shows observed used percentage and a dotted forecast to exhaustion or reset, whichever comes first. The projected time is labelled before, at or after reset. |
| **Context** | Is the 3-layer window healthy and cheap? | Compare request input with the latest recorded W threshold. Ticks mark rebuilds. A rebuild can have several causes. Compare cache reads immediately after a rebuild with steady state and read the recorded proportions of the latest window. Extended history in L3 has no recorded token size. [Three-layer context measurements](context-3layer.md#measuring) |
| **Agents** | Which children cost the most, which failed, which are stuck? | Scan the weighted-token ranking for each child's result and cache share. Duration is relative to the longest child. Time since last activity is highlighted above 15 minutes and again above 45 minutes for running children. |
| **Tools** | Where does tool time go and what fails? | Compare each tool's share of timed calls and its failure rate among completed or failed calls. The longest five calls have their own bars. |
| **Time** | Where did wall-clock time go? | Check the main Session's total and hourly stacks. Overlapping intervals count once, with approval and usage-limit waits first, then recorded child waits, tools and generation. Unattributed time marks gaps in phase evidence. Identical consecutive hours are grouped. Press **E** to toggle raw events. |
| **Errors** | What is failing now? | Compare the last hour with the Session total by error kind. The newest recorded failures are first. |
| **Turns** | Which turns used the most input and output? | Each bar splits cached input, other input and output. Lengths are relative to the largest turn. |
| **Export** | How can I save analysis data? | Save metadata and statistics as JSON or Markdown. The exports retain detailed measurements for archived children and historical summary strategies. |

**Tokens.** Weighted use adds uncached input, cache writes and output. Cache reads have their own series. Weights rank token use without pricing it. Usage and Agents include recorded descendants at any depth. Claude's per-model usage breakdown replaces the corresponding turn totals when available. The last 12 hourly buckets feed the token chart. The current incomplete hour is plotted separately from the EMA. The EMA uses alpha **0.35** per completed hour, starts again after gaps, and projects the latest smoothed rate for the next **3 hours** as a dotted line.

**Allowances.** Unreal uses observations from the main Session and its descendants, including archived ones. These percentages describe the provider account. The burn rate is in percentage points per hour, using up to **3 hours** of observations up to the latest observation in the same reset cycle. Its EMA uses alpha **0.35** for a one-hour interval, adjusted for the time between observations. The estimate starts at the latest observation. After reset, the gauge is `—` until a new observation arrives.

**Signals.** Cache read share is OK at **90%** or higher, warning at **75%** to below **90%**, and bad below **75%**. Allowance warnings begin at **20%** remaining and turn bad at **10%**. Failure rate warns at **5%** and turns bad at **15%**. These are display thresholds. `—` means the value is unavailable. `~` marks an observed subtotal or a comparison with incomplete coverage. On narrow screens, rows keep their bars on one line and shorter table columns may be omitted.

### Context budget errors

`required_context_exceeds_budget` reports the input budget and the sizes of the system and tool header, the L2 state copy, and the required L1 tail before and after attempting to fit it. Use the breakdown to identify whether the active request, tool output, or the state copy accounts for the size. If a provider rejects an already admitted request for its context size, Unreal rebuilds the window and retries once. [Three-layer window settings](context-3layer.md#rebuild-the-responses-request-window)

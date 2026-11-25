# Usage

**Language:** English | [日本語](ja/usage.md)

This guide covers daily usage with the **installed common-kit `unreal` command**. (For flags on the source-built binary, see [Architecture — Native launcher](architecture.md#native-launcher).)

---

## Starting and Finding Sessions

A **Session** is a persistent conversation tied to a workspace (your Git worktree root, or the current directory outside Git).

1. Open Bash in your project directory and run:
   ```bash
   unreal development
   ```
2. If no Session named `development` exists yet, enter `y` to create it.
3. Later, run `unreal development` from **any directory** to reconnect to that Session.

### CLI Commands

| Command | What It Does |
| --- | --- |
| `unreal` | Lists existing main Sessions (newest first) so you can pick one by number |
| `unreal <name>` | Attaches to `<name>` from any directory; the Session runs in the workspace where it was created (offers to create it in the current workspace when no Session has that name) |
| `unreal --where <name>` | Prints the workspace directory path for Session `<name>` |
| `ucd <name>` | Changes your current Bash directory (`cd`) to Session `<name>`'s workspace |
| `unreal --workspace-info` | Prints Workspace, Config, State, and Socket for the current workspace |
| `unreal --readme` | Displays the README bundled inside the installed kit |

> **Notes:**
> - Running bare `unreal` without a name only lists existing Sessions; it never creates a new one.
> - **A Session name refers to exactly one Session.** Names never coexist: calling a name from any directory works in that Session's original workspace, and a new Session cannot take a name that is already used (not even with `--instance`).
> - One workspace keeps one instance: kit updates and provider settings changes reuse it.
> - If an older version left several Sessions with one name, opening it prints a warning and a list (Enter opens the newest one with a running Host). Retire the extra ones with `unreal --archive-session <name> --workspace <path>` (add `--instance` when one workspace has several); records are kept, and a retired name can be used for a new Session.
> - Advanced flags `--instance` and `--config` target an explicit operator instance in the current workspace and bypass global Session lookup; they still refuse a name used by another Session.

---

## Working in the Terminal (TUI)

### Basic Workflow

1. **Slash commands:** Type `/` to open the command menu. Use `Up`/`Down` to navigate, `Tab` to autocomplete, and `Enter` to select.
2. **Choose model & permissions:**
   - `/model` — Switch provider (`Claude` or `Codex`), model, and reasoning effort.
   - `/permissions` — Switch between `Read Only`, `Ask for Approval`, and `Full Access`.
3. **Send tasks:** Type your message and press `Enter`.
4. **Navigate:** Use `Page Up` / `Page Down` (or mouse wheel) to scroll history. Run `/help` at any time to view commands and shortcuts.

### Views (`/view`)

- **Split (default on terminals ≥120 columns):** Shows **Chat** on the left and the **Work pane** (live main/child agent cards) on the right. Automatically switches to Chat if you narrow the window below 120 columns.
- **Chat:** Focuses on the conversation. Children appear in a compact 1-line dock above the input bar at every width. The right Work pane is reserved for Split.
- **Orchestration:** Uses the full main area to display agent cards and progress beads. Scroll with `Page Up` / `Page Down`.

### Footer Gauges

The bottom bar displays three live metrics (`—` when unknown):
- **Context (`文脈`):** Current input token usage and percentage of the model's context window.
- **Speed (`速度`):** Output generation speed in `tok/s`. A `~` prefix indicates client-clock timing (used for Claude). Tool execution and approval wait times are excluded.
- **Cache (`キャッシュ`):** Prompt cache hit rate for the latest turn.

The footer also shows the remaining ChatGPT and Claude subscription allowance from the Session tree, including archived children (for example `週 残り13%`; ASCII: `7d left 13%`). Common windows use short labels such as `5h` and `週` (weekly). Reset times use the terminal's local time. As space runs out, the footer drops reset times and optional fields. Open **Subscription usage** in `/analyze` for observation times and windows whose reset has passed. When a provider reports charges, the footer also shows today’s charged amount and the current rate. This fits after the subscription gauges and is the first item to disappear on narrow terminals.

---

## Inspecting Session usage

From a Session shell, run `unreal-agent usage`. Add `--since 30m` for a custom recent window or `--json` for a supervisor script. The ledger includes the caller and all descendants, including archived children. It reports total, 10-minute and 60-minute calls and token counters, provider/model and role breakdowns, top children, charges and allowance forecasts when observed. A partial total includes only available records. Unreported charges remain unknown.

To inspect an offline copy, pass `--session-directory PATH --session ID` together. Point to its `sessions/` directory with `sessions-archive/` beside it. This reads the logs without opening a Host or writing to the copy.

## Native File Tools

Providers with Unreal's native tools can read several files and line ranges in one `read` call. Prefer one batch for the evidence needed for the current step. Each file returns its own status and whole-file revision. Ranges are 1-based. Omitting `ranges` reads the file from line 1 within the shared output budget. The single-file `path`, `start_line`, and `lines` arguments still work.

```json
{"files":[{"path":"main.go","ranges":[{"start_line":20,"lines":40},{"start_line":120,"lines":25}]},{"path":"config.go","ranges":[{"lines":50}]}],"max_bytes":24000}
```

Set `context_lines` on `grep` to get surrounding lines in the same response as each match. `before` and `after` list preceding and following lines in file order.

```json
{"path":".","glob":"**/*.go","pattern":"func build","context_lines":5}
```

Each call shares one `max_bytes` allowance across its results. The default is 16 KiB and the maximum is 64 KiB. When `truncated` is true, the call stopped at its output limit. The receipt's `locator` can retrieve the bounded result from L3 after the context window excerpts it. [Three-layer evidence reads](context-3layer.md)

## Keyboard Shortcuts & Text Input

| Key / Action | Effect |
| --- | --- |
| `Enter` | Send message or run slash command |
| `Ctrl+Enter`, `Shift+Enter`, `Alt+Enter`, or `Ctrl+J` | Insert a newline (if supported by your terminal) |
| `\` at end of line + `Enter` | Insert a newline (works in all terminals) |
| `@` | Search and insert workspace file paths |
| `Ctrl+A` | Select all text in the input box |
| Type / Paste while selected | Replace selected text |
| `Backspace` / `Delete` while selected | Clear selected text |
| `Esc` or Arrow keys | Clear selection |
| `Home` / `End` | Move to start / end of input (or top / bottom of chat when input is empty) |
| `Page Up` / `Page Down` | Scroll conversation or Orchestration cards |
| `Ctrl+D` | Detach from TUI (Session keeps running in the background) |
| `Ctrl+C` | Cancel pending submission wait; otherwise stop the Session |

### Pasting and File Drop

- **Multiline paste:** Long pastes collapse into `[Pasted text #1 +N lines]` in the input box and never auto-submit until you press `Enter`.
- **Windows Sandbox file drop:** You can copy [`scripts/windows/sandbox-drop.cmd`](../scripts/windows/sandbox-drop.cmd) into your Windows `SendTo` folder and rename it to `<sandbox-name>.cmd` (e.g., `development.cmd`). Dropping files onto it copies them into `/home/agent/drop/` inside the Sandbox and places their paths on your clipboard.

---

## Copying and Exporting Conversations

### Copy to Clipboard (`/copy`)

- `/copy` — Opens an interactive message picker. Use `Up`/`Down` to move, `Space` to toggle multiple messages, and `Enter` to copy.
- `/copy last` — Copies the latest public agent response.
- `/copy all` — Copies the entire public conversation as Markdown.

> **Note:** `/copy` uses the OSC 52 terminal escape sequence. If your terminal or multiplexer (`tmux`/`ssh`) blocks OSC 52 clipboard writes, use `/export` instead.

### Export to Markdown File (`/export`)

- `/export` — Opens a menu to export the **Latest response**, **Full conversation**, or **Choose a response**.
- Direct shortcuts: `/export last`, `/export all`, `/export pick`.
- Files are written to `${XDG_STATE_HOME:-$HOME/.local/state}/unreal-agent/exports` and the exact path is printed in the TUI.

Both `/copy` and `/export` read from the complete canonical history (even messages scrolled off-screen) and omit internal reasoning, compaction markers, and routine background child traffic.

---

## Stopping, Detaching, and Resuming

### Stopping Work (`/stop`)

Running `/stop` opens a menu with four scopes (**Main agent** is focused by default):

| Option | Shortcut Command | What It Stops |
| --- | --- | --- |
| **Sub-agents** | `/stop sub` | Cancels all running child agents; main agent continues |
| **Main agent** | `/stop main` | Interrupts the main agent's current turn; children continue |
| **Main agent and sub-agents** | `/stop main+sub` | Interrupts the main agent and cancels all children |
| **Session** | `/stop session` | Stops the entire Session (resume later with `/resume`) |

### Detaching and Resuming

- **Detach (`Ctrl+D` or `/detach`):** Closes the terminal UI while the Session continues running in the background.
- **Resume (`/resume` or `unreal <name>`):** Resumes a stopped Session and restores your last selected permission mode. If the previous provider is unavailable, switch providers with `/model` first and then run `/resume`.
- **Retry (`/retry`):** If a message submission fails or times out with an uncertain state, `/retry` safely resends it using the original submission ID so it is never duplicated.

---

## Delegated Background Work (Child Agents)

The main agent can delegate implementation or research to Unreal children using templates such as `lead`, `worker-*`, and `collector`. Claude Code calls `SubagentStart` through Bash with `unreal-agent subagent start`. The built-in `Agent` and `Task` tools are disabled. A Claude child sends questions with `unreal-agent subagent send-parent` and delivers its final reply as the completion report.

For a single start, add `--model deepseek-v4.1-flash --effort max`. Add `--provider opencode-go` to choose that connection explicitly. Use `subagent model set` to save role choices for the calling Session and its future delegation. See [Choosing subagent models](providers.md#choosing-subagent-models).

- **Automatic wake-up.** User input, a child completion notice, or a forwarded question or approval wakes an idle parent, including after a failed parent model turn. During a usage-limit wait, the parent picks up queued reports when the wait ends. Ready and progress notifications stay quiet.
- **Compact chat display:** Routine parent-child coordination is folded into a single summary line (`┄ 裏の作業 ...`).
- **Important notices stay visible:** Agent messages starting with `【報告】` (Report) or `【要確認】` (Needs Confirmation) are always shown in full, and `【要確認】` highlights a banner above the input bar until you reply.
- **Approvals:** Child tool requests follow the parent Session's permission mode and are mediated by the parent agent. See [Providers — Delegated GPT work](providers.md#delegated-gpt-work) and [Permissions](permissions.md).

### Delegation brief

`SubagentStart` accepts an optional `brief` with `title`, `purpose`, `inputs`, `outputs`, `verification`, `acceptance_criteria`, and `budget_steps`. The Bash command accepts the same fields.

```bash
unreal-agent subagent start --template lead --title 'Check report totals' \
  --purpose 'Decide whether to accept the report' --inputs 'report.json' \
  --outputs 'A list of discrepancies' --verification 'Recompute the totals' \
  --acceptance-criteria 'Cite each discrepancy and its source' --budget-steps 40 --task 'Use the current fixture.'
```

Free-text `task`, `--task`, and a quoted heredoc still work. Omitted fields appear as `未指定/unspecified`. The harness adds the absolute work root and any differing process working directory, the selected permission mode and configured write scope, `TMPDIR`, restrictions from the settings and explicit instructions, the report format, certainty labels with sources, and a next-action placeholder. An explicitly labelled `Prohibitions: …` line from the task or acceptance criteria is reproduced in the brief. The child receives the same text shown in the start preview and in `subagent show HANDLE --task`.

A child can ask `SendParent` for the exact value of one field using `brief <field>`, such as `brief root` or `brief verification`. The tool returns the stored value when present. Other questions go to the parent unchanged. The ended notice includes the full recorded completion report. The parent reviews the evidence and decides whether to accept.

`/analyze` → **Overview** shows measured handled and forwarded counts per routing rule. It labels the estimated parent wakeups saved as an upper bound from brief replies, since multiple messages can share a parent turn.

### Budgets, notices and queued children

`--budget-steps N` sets the expected model calls across the child and its descendants. Omit it or use zero for no budget. Start reports the selected provider, model, effort, budget and usage by provider in the caller’s tree. The child is asked to tell the parent before exceeding the budget.

At 80%, the child receives a notice. At 100% and each further 50 percentage points of the original budget, the parent receives a waking notice with the subtree call count and provider breakdown. Without a budget, 250, 500, 1,000 and each further doubling are reported as quiet progress in the parent’s next request. Notices are recorded so each threshold is delivered once across restarts. The model or operator decides whether to continue, stop or split the work.

`subagent list`, `show` and `wait` report each child’s subtree calls, budget, charge and state. Native callers use `SubagentStart` with `action` set to `list`, `show` or `wait`, and `template` set to a name or `"*"`. Pass `handle` for `show`, and optionally for `wait`. `list` accepts `all: true` for ended children.

When [the concurrency limit](install.md#limiting-concurrent-children) is set and the tree has no free slot, the start returns a queued handle and its initial position. The tree takes queued starts in order as slots free up. Queued handles work with `list`, `show`, `wait` and `cancel`, and the queue is restored when the Host reopens the Session. Running parents below the root also occupy slots, including while waiting for their children.

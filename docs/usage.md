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
| `unreal <name>` | Attaches to `<name>`, or offers to create it in the current workspace |
| `unreal --where <name>` | Prints the workspace directory path for Session `<name>` |
| `ucd <name>` | Changes your current Bash directory (`cd`) to Session `<name>`'s workspace |
| `unreal --workspace-info` | Prints Workspace, Config, State, and Socket for the current workspace |
| `unreal --readme` | Displays the README bundled inside the installed kit |

> **Notes:**
> - Running bare `unreal` without a name only lists existing Sessions; it never creates a new one.
> - If the same Session name exists in multiple workspaces, the current workspace takes precedence; otherwise Unreal prompts you to pick by number.
> - Advanced flags `--instance` and `--config` target an explicit operator instance in the current workspace and bypass global Session lookup.

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

The footer also shows the remaining ChatGPT and Claude subscription allowance from the Session tree, including archived children (for example `週 残り13%`; ASCII: `7d left 13%`). Common windows use short labels such as `5h` and `週` (weekly). Reset times use the terminal's local time. As space runs out, the footer drops reset times and optional fields. Open **Subscription usage** in `/analyze` for observation times and windows whose reset has passed.

---

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

The main agent can delegate implementation or research to configured child templates such as `lead`, `worker-*`, and `collector`. Tool-capable agents use `SubagentStart`. Claude Code can start Unreal children in Bash with `unreal-agent subagent start`.

- **Automatic wake-up.** User input, a child completion notice, or a forwarded question or approval wakes an idle parent, including after a failed parent model turn. During a usage-limit wait, the parent picks up queued reports when the wait ends. Ready and progress notifications stay quiet.
- **Compact chat display:** Routine parent-child coordination is folded into a single summary line (`┄ 裏の作業 ...`).
- **Important notices stay visible:** Agent messages starting with `【報告】` (Report) or `【要確認】` (Needs Confirmation) are always shown in full, and `【要確認】` highlights a banner above the input bar until you reply.
- **Approvals:** Child tool requests follow the parent Session's permission mode and are mediated by the parent agent. See [Providers — Delegated GPT work](providers.md#delegated-gpt-work) and [Permissions](permissions.md).

### Delegation brief

`SubagentStart` accepts an optional `brief` with `title`, `purpose`, `inputs`, `outputs`, `verification`, and `acceptance_criteria`. The Bash command accepts the same fields.

```bash
unreal-agent subagent start --template lead --title 'Check report totals' \
  --purpose 'Decide whether to accept the report' --inputs 'report.json' \
  --outputs 'A list of discrepancies' --verification 'Recompute the totals' \
  --acceptance-criteria 'Cite each discrepancy and its source' --task 'Use the current fixture.'
```

Free-text `task`, `--task`, and a quoted heredoc still work. Omitted fields appear as `未指定/unspecified`. The harness adds the absolute work root and any differing process working directory, the selected permission mode and configured write scope, `TMPDIR`, restrictions from the settings and explicit instructions, the report format, certainty labels with sources, and a next-action placeholder. An explicitly labelled `Prohibitions: …` line from the task or acceptance criteria is reproduced in the brief. The child receives the same text shown in the start preview and in `subagent show HANDLE --task`.

A child can ask `SendParent` for the exact value of one field using `brief <field>`, such as `brief root` or `brief verification`. The tool returns the stored value when present. Other questions go to the parent unchanged. The ended notice includes the full recorded completion report. The parent reviews the evidence and decides whether to accept.

`/analyze` → **Overview** shows measured handled and forwarded counts per routing rule. It labels the estimated parent wakeups saved as an upper bound from brief replies, since multiple messages can share a parent turn.

# Context and Project Instructions

**Language:** English | [日本語](ja/context.md)

Each model request uses a three-layer context across providers and agent roles. L1 holds the current conversation window, L2 holds a copy of structured Session state at each rebuild, and L3 gives access to the original records and captured outputs. [Three-layer context and settings](context-3layer.md)

## 1. Request Windows

Unreal fixes system instructions, tool schemas and the L2 copy at a rebuild. New messages and tool receipts append in recording order. Model or provider changes can start a new window. Claude Code starts a fresh conversation at each rebuild and resumes it by ID between rebuilds. [Claude Code window settings](context-3layer.md#rebuild-the-claude-code-window)

When the required tail would exceed the input budget, Unreal fits long text with head and tail excerpts and links to L3. The window records the fitted size. If the smallest projection still exceeds the budget, the error lists the input budget and the sizes of the system and tool header, L2 and the L1 tail. A provider context overflow triggers a rebuild and one retry.

## 2. Token Accounting

Unreal compares the total input estimate with the model context size after response and protocol reserves. An unknown model window uses `FallbackWindow` (272,000 tokens by default). If available on the same prefix, reported input plus the estimated change governs admission. Claude uses its latest model call's input, including cache reads and writes.

Each Turn records `ContextAdmission` with the estimate, measured prefix, delta, adopted count, budget, estimator and request size. The text estimate uses four ASCII characters per token and one token per non-ASCII rune. Tool schemas count once. Images use a size heuristic where usage is unavailable.

---

## 3. Project Instructions (`AGENTS.md` and `CLAUDE.md`)

Project instructions guide coding style and workflow. They never grant tool permissions, credentials, or Session authority.

### How Codex Reads Instructions

- **Snapshot at Session creation:** When creating a new Session, Unreal reads `<workspace>/AGENTS.md` ([`discover.go`](../harness/projectinstructions/discover.go)) and saves its exact text as an immutable snapshot in the Session store.
  - A missing `AGENTS.md` records no instructions; a present but unreadable or invalid file fails new-Session creation.
  - A legacy Session without a stored instruction snapshot does not discover a new `AGENTS.md` on resume.
- **Consistent across resume & children:** Resuming, restarting, forking, or spawning child agents from that Session reuses the saved snapshot. Editing `AGENTS.md` on disk takes effect when you create a new Session.
- **Format & validation rules:**
  - Must be valid UTF-8 with no NUL bytes. Nested `AGENTS.md` files in subdirectories and `@import` directives are not expanded by the snapshot reader.
  - Discovery uses `os.ReadFile` (it does not promise no-symlink reads or a file-size limit).
  - Loading validates the snapshot schema, not a recomputed content digest or byte length.

### How Claude Reads Instructions

- Claude Code reads `CLAUDE.md` natively.
- On each `claude` CLI invocation, Unreal also reads the workspace's current `AGENTS.md` and appends it to the prompt—**unless** `CLAUDE.md` already contains the substring `AGENTS.md` (such as `@AGENTS.md`). See [`prompt.go`](../harness/llm/clients/claudeagent/prompt.go).
- The `AGENTS.md` substring check is only a heuristic; it does not prove that Claude Code actually imported the file.
- Because Claude reads the file on each invocation, edits to `AGENTS.md` or `CLAUDE.md` take effect on subsequent turns even within an existing Session.

---

## 4. Inspecting Context & Usage Metrics

Open `/analyze` → **Context** for the input chart, rebuilds and cache comparisons. **Export** includes excerpt fits, evidence reads and historical summary usage and outcomes.

See [Troubleshooting — Read analysis and measurements](troubleshooting.md#read-analysis-and-measurements) for details on interpreting these numbers.

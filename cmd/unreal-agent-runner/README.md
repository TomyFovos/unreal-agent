# unreal-agent-runner

**Language:** English | [日本語](../../docs/ja/cmd/unreal-agent-runner/README.md)

`unreal-agent-runner` executes a single prompt or JSON request headlessly, streams public Session items and runner events to `stdout` as JSONL, and exits when the task finishes.

> **Intended Use:** This is a developer and benchmark evaluation tool. For interactive daily development with subscription logins, use the [common-kit terminal (`unreal`)](../../docs/install.md).

---

## 1. Build and Run

Requires the Go version in [`go.mod`](../../go.mod) and a C compiler (`CGO_ENABLED=1` for Tree-sitter AST tools).

From the repository root:

```bash
make build
bin/unreal-agent-runner -h
```

### Running a Prompt with an OpenAI API Key

By default, the runner uses the `openai` provider. (API-key routes are for evaluation and incur API charges; they are separate from the interactive TUI's subscription setup.)

To enter an evaluation API key without recording it in your shell history:

```bash
read -r -s -p 'Evaluation OpenAI API key: ' OPENAI_API_KEY
printf '\n'
export OPENAI_API_KEY
bin/unreal-agent-runner -p 'Inspect this project and explain how to run its tests.'
```

### Specifying a Workspace and Saving JSONL Output

```bash
bin/unreal-agent-runner -workspace . -p 'Summarize this project.' > run.jsonl
```

> **Security Note:** `unreal-agent-runner` is not an OS sandbox. Use an appropriately isolated development/evaluation environment; a container or VM is recommended when OS isolation is needed.

---

## 2. JSON Requests and Configuration

Instead of `-p`, you can pass a structured JSON request as a positional argument or via `stdin`:

```bash
bin/unreal-agent-runner '{"prompt":"Summarize this project."}'
printf '%s\n' '{"prompt":"Summarize this project."}' | bin/unreal-agent-runner
```

### Request Schema & Flags

- Flags must appear before a positional JSON argument (`-p` and positional JSON cannot be combined).
- Unknown JSON fields are rejected.
- Common JSON fields: `prompt`, `messages`, `model`, `thinking_level`, `session_id`, `system_prompt`, `max_attempts`, `disallowed_tools`, `profile` (run `bin/unreal-agent-runner -h` for the complete schema).
- **Output stream vs. canonical store:** JSONL on `stdout` is a client view and may be incomplete if writing to `stdout` fails; the persisted Session store remains the canonical record.

### Storage and Provider Environment Variables

- **Session store:** Defaults to `${XDG_STATE_HOME:-$HOME/.local/state}/unreal-agent/sessions` (override with `-session-directory`). Passing a `session_id` creates or resumes a persisted Session.
- **Optional log directory:** Use `-log-directory <dir>` to write per-run JSONL logs.
- **Provider & model selection:**
  - `UNREAL_HARNESS_LLM_PROVIDER`: `openai` (default), `openai-codex` (uses external Codex OAuth), `openrouter`, `fireworks`, or `ollama`
  - `UNREAL_HARNESS_LLM_MODEL`: Model identifier for the selected provider

See [provider definitions](../../cmd/internal/agentrunner/providers.go) and the [Harbor Benchmark Adapter](../../benchmarks/harbor/README.md) for full benchmark setup.

## 3. Harness-Lab Profiles and Measurements

`profile` (JSON takes precedence over `-profile`) selects runner composition:

- `conservative` (default) preserves the existing runner tools and execution path.
- `development` uses the product RuntimeFactory, native file operations,
  Tree-sitter `ast_grep` / `ast_edit`, Bash, ViewImage, discovered SkillUse,
  configured language/debug tools, and the product child manager. Roles are
  `lead`, `worker-low`, `worker-medium`, `worker-high`, `worker-xhigh`,
  `worker-max`, and `collector`. Lead can start workers and collectors.

The existing `profile_id` / `profile_version` select the versioned prompt
profile independently. For example, `profile_id: "openai-reasoning"`,
`profile_version: 1`, `model_family: "openai-reasoning"` retains that profile's
existing validation and prompt composition.

For a same-model measurement, all roles use the requested model, provider and
endpoint. Product role prompts and effort levels are reused (lead: max,
collector: medium, workers: their named effort); model names in the lead prompt
follow the measurement model. `thinking_level: "none"` omits reasoning settings
for the parent and all descendants. Without it, the parent's default stays high.
The normal product's 45-second child-report batching is preserved.

### Transport

`UNREAL_HARNESS_LLM_HEADERS` accepts a JSON object of string values, or
comma/newline-separated `Name=Value` pairs (`Name: Value` also works). For example:

```bash
export UNREAL_HARNESS_LLM_HEADERS='{"User-Agent":"my-harness","x-opencode-session":"conversation-id"}'
```

Defaults are `User-Agent: unreal-agent-runner/<REV>` and a transport-only
`x-opencode-session` derived from the Session ID. It is stable across turns and
resuming the same `session_id`; a new child has its own conversation ID. Explicit
headers override the defaults and are inherited by children. Header values and
credentials stay in the transport/process environment, outside runtime identity
and JSONL. Invalid-header diagnostics omit supplied names and values.

The caller's `SANDBOX_EGRESS_PROXY` is honored. An entry in workspace `.env`
overrides it, including an explicitly empty entry. The proxy and headers are
client-local; process-global proxy variables are unchanged.

### JSONL

Existing public Host records retain `Kind` and `Data`. The runner attachment
assigns monotonically increasing `Sequence`; `SourceSequence` keeps the source
history position. Additional records use `Synthetic: true`:

- `subagent_start` / `subagent_end`: `Data.CallID`, `Template`, `Role`, `ChildID`,
  and (on completion) `Status` and available `Summary`. These follow accepted
  Operations and their outcomes, including failure/cancellation.
- `file_change`: `Data.Path`, `Change` (`add` / `modify`), `Tool`, `CallID`.
  Applied native/AST receipt targets count; rejected edits and AST previews do
  not. Paths are normalized and emitted once across the parent and descendants.
- Child `model_response` and `tool_call_status` records carry top-level `ChildID`.
  Usage (including raw provider usage) and tool calls include every descendant,
  once, using paged canonical history after the owner drains. Child assistant
  messages are omitted so they cannot replace the parent's final answer.
- Success ends with `Kind: "session_end"`, `Data.IsError: false`, aggregate
  `Usage`, `ToolCalls`, and sorted unique `ChangedFiles`.
- Failure/cancellation ends with `{"type":"session_end","is_error":true}`;
  an `error` record precedes it when an error is available. Exit codes are 0
  (success), 1 (failure), and 130 (cancellation). Help/version are plain CLI
  operations and do not emit session records. Write failures propagate; an
  unwritable output or SIGKILL cannot carry a terminal record.

Bash tool calls are included. File changes performed solely inside arbitrary
Bash commands do not have native mutation receipts and are not synthesized as
`file_change`, matching the reference patch's scope.

### Packaging

The existing root Dockerfile is the runner image, including Bash, CA certificates,
git and tini. Set its source revision when building without `.git`:

```bash
docker build --build-arg REV="$(git rev-parse HEAD)" -t unreal-agent-runner .
make snapshot GORELEASER=/path/to/goreleaser
```

Snapshots include all product binaries and a Linux amd64 `.tar.gz.sha256`
sidecar, in addition to GoReleaser's `SHA256SUMS`. The snapshot smoke checks the
runner's exact REV and its native AST operations against a local fake provider.

# Claude in `/model` (the Claude Code route)

`Claude` in `/model` runs the official Claude Code CLI on the user's own
Claude subscription login (U21). It uses no MCP server, mod or API key.

## Why Claude Code, not a direct connection

Unreal first tried to call Claude itself with the user's subscription login
(OAuth), keeping its own tools and Operations. That route was dropped:

- Anthropic's terms reserve subscription OAuth for Claude Code and
  Anthropic's own apps; other products are pointed to API keys or supported
  clouds
  ([Authentication and credential use](https://code.claude.com/docs/en/legal-and-compliance#authentication-and-credential-use)).
- In practice, requests from Unreal's own client with that login were
  rejected (HTTP 429).
- An API key was not an option: Unreal uses subscriptions only, never metered
  API billing (U5).

So Unreal runs the official Claude Code with the user's login instead, and
keeps approvals, records and orchestration on its side (U21).

## Requirements

- Install Claude Code where Unreal runs and sign in once: run `claude`, then
  `/login` with the Claude subscription account.
- Unreal finds `claude` from, in order:
  1. the configured binary;
  2. `PATH`;
  3. `~/.local/bin/claude`.
- Claude is offered when Claude Code reports a subscription plan. Discovery
  reads only Claude Code's `initialize` reply: it starts no conversation and
  uses no plan quota.
- `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN` and `CLAUDE_CODE_USE_*` are
  removed from Claude Code's environment, so only the subscription is used
  (U5).

## How a turn runs

Each Unreal turn starts Claude Code and continues the Session's own
Claude Code conversation: the first turn uses `--session-id`, later turns use
`--resume`.

After Claude's `Respond` returns and the Session has no model call or pending
tool/operation, Unreal checks the last context size reported by Claude Code:
the last model call's input + cache read + cache creation, with `result.usage`
as a fallback when no per-call measurement is available. Above **200,000
tokens**, it immediately resumes the same conversation with `/compact` in the
background. Configure this with `Context.ClaudeCompactThresholdTokens` in the
runtime JSON; zero or an omitted value uses the default. The threshold is
provisional and will be revisited using production observations. This setting
is outside immutable Session identity, so changing it does not require a new
Session. Tool batches and input steering within a turn never trigger compaction.

New user input or a child notice that starts a turn stops and reaps the compact
process before the real turn starts. An interrupted summary is not saved; a
short Session record and log note the interruption and retain the previous
context size. If it still exceeds the threshold at the next idle boundary,
Unreal tries again. There is at most one Claude Code process for the same
conversation, across both compaction and ordinary turns.

At turn start, compaction remains only as an emergency fallback when the last
context size is **strictly greater than twice the configured threshold**
(400,000 tokens with the default). It then waits for `/compact` before sending
the input. At or below that value, the new turn does not wait for compaction.

The English compact instruction preserves the goal, decisions, unanswered user
questions, active child handles and roles, files being changed, and next steps.
The header and main card show 「会話を圧縮中」 with a spinner, and the input
field remains usable. A short Session context record stores the
before/after token counts (unreported after counts stay unknown), and the last
reported size survives Host restart. The summary stays in Claude Code's own
conversation; Unreal's canonical messages and receipts are preserved. Failures
are logged with their cause, and the original input still runs without compacting.

The gauge also uses the last model call's input + cache read + cache creation
for 「文脈」, even when several calls occur between recorded responses. Cache
ratios and `/analyze` totals continue to use the summed usage of those calls.

- **Tools:** Claude Code runs its own tools (Read, Edit, Write, Bash and the
  rest).
- **What Unreal records:** Unreal shows the text as it streams. It records
  every tool call and the result Claude read as a `claude_code` receipt in the
  Session.
- **After switching models:** if another model (such as Codex) answered in the
  meantime, those turns are sent to Claude as context. Codex sees Claude's
  turns the same way.
- **Input during work:** new user messages in a foreground Claude turn go
  into the next `PostToolBatch` (`turn.batch`) hook's `additionalContext`.
  Multiple messages are delivered together, with credential redaction and
  terminal control removal. The Coordinator projects them into that turn and
  persists its continuation input watermark, so the completed turn does not
  resend them. User input still interrupts a background turn and starts a new
  foreground turn. The Codex route keeps its existing steering behavior.
- **Child waits:** interruption recognizes the adjacent words `subagent wait`
  anywhere in a Bash command, including variables, substitutions, `timeout`
  and pipes. It also checks for pending user input when the wait begins.
  This hint does not grant approval: the strict literal command check still
  decides whether orchestration can skip an approval request. It does not
  impose a wait timeout.
- **Read child results:** `ended` notices wait until the current model call
  finishes. If a recorded `subagent wait` or `subagent show` result already
  contains that child's final report, Unreal suppresses its notice. Unread
  completions, child messages and approval requests keep their existing paths.
- **Child progress:** children send intermediate updates with
  `SendParent {"text":"...","progress":true}`. These are recorded as
  `progress` peer messages and count as reports in the TUI, but do not start a
  parent model call or interrupt idle compaction. The updates join the context
  when a question, completion or user message next wakes the parent. Questions
  and requests needing a decision omit `progress` (or set it to `false`) to
  wake the parent as before. The `subagent send` command from Claude to a child
  keeps its existing behavior.
- **Stopping:** `/stop main` ends the model call and its own tools, clears its
  progress/pulse, and waits for the next user message. Children continue.
  Subscriber eviction drains queues without waiting while holding the Session
  mutex, so concurrent gateway reads cannot freeze submission or inspection.
- **Host restart:** unfinished child start operations are canceled without
  respawning. A durable `ended` peer message tells the parent that Unreal Agent
  restarted and points to `subagent show HANDLE` for the partial report. Its
  stable input ID prevents duplicate notices across restoration. Claude handles
  that message through the existing background-turn path.

## Approvals

Claude Code starts with `--disallowedTools Agent Monitor AskUserQuestion`.
Whenever Claude asks for or awaits a user reply, including in ordinary user
turns, its instructions require a message beginning with `【要確認】` and choices
written as numbered prose. The attention band keeps that question visible until
the next user input, even as background replies arrive.

Claude Code asks Unreal before a call that needs approval. Unreal's permission
mode decides:

| Mode | Edit, Write, Bash and other changes |
|---|---|
| Full Access | allowed |
| Ask | shown to the human in the TUI with the tool and its target, such as `Claude Code Edit: sample.txt` |
| Read Only | denied; Bash is denied before Claude Code's own rules, except commands that only read (docs/permissions.md), which run in every mode |

Claude Code's own rules still apply first:

- **Allowed without asking Unreal:** reads inside the workspace, and commands
  Claude Code's settings allow.
- **Declined by Claude Code:** commands its settings do not run.

## Commands Claude Code declines (U22)

When Claude Code declines a shell command itself, Unreal handles it as an
ordinary Bash request of the Session:

1. Unreal's permission mode decides, as for any Bash call: Full Access runs it,
   Ask asks the human, Read Only denies it.
2. If allowed, it runs in Claude Code's working directory as a normal Unreal
   Bash Operation, recorded in the Session.
3. Its result goes back to Claude in the same turn.

Codex commands go through the same permission mode.

## Models and errors

- **Models and effort:** `/model` lists the models Claude Code offers the
  login. Effort is passed as `--effort`.
- **Server-side refusals:** a usage limit or another refusal is shown as
  Claude Code's own message (U18).
- **Sign-in problems:** an expired sign-in shows as "auth required". Run
  `claude` and `/login` again.

## Live checks

These tests use the signed-in subscription with Haiku, so they are opt-in:

```bash
UNREAL_LIVE_CLAUDE=1 go test ./cmd/unreal-agent -run 'TestLiveClaude' -v
```

## GPT subagents (Claude designs, GPT implements)

When Codex is registered, every Session gets these child templates:

- `lead`: gpt-6.1-sol at max effort. It may start workers itself. Requests
  carry max; delegation comes from the lead's prompt and workers.
- `worker-low`, `worker-medium`, `worker-high`, `worker-xhigh`, `worker-max`:
  gpt-6.1-sol at that effort. Workers start nobody.

Unreal does not offer ultra, even when the Codex catalog includes it. Existing
Session and child settings using ultra are read and displayed as max. Requests
use max, or the model's highest supported effort when max is unavailable;
Codex's `multi_agent_reasoning_effort` does not determine this value.

Claude has no Unreal tools, so it drives children with the Host's own command
through Bash, without MCP or a mod. Claude Code gets `UNREAL_AGENT_SOCKET` and
`UNREAL_AGENT_SESSION` in its environment, and its prompt names the exact
command:

```
<unreal> subagent start --template lead <<'EOF'
goal, design, files and interfaces, constraints, how to verify
EOF
<unreal> subagent wait [HANDLE ...] [--timeout 9m]
<unreal> subagent approve HANDLE REQUEST allow|deny
<unreal> subagent send HANDLE <<'EOF' ... EOF
<unreal> subagent cancel HANDLE | list | show HANDLE
```

Unreal approves these commands itself in every mode, but only in this form:
literal words, optionally followed by a quoted heredoc. Anything else (pipes,
`;`, expansions, redirections) goes through the normal approval.

Children follow the Session's permission mode (docs/permissions.md). In Ask,
the lead's requests come to Claude (`wait` lists them), and the workers'
requests go to the lead, which answers with SubagentApprove.

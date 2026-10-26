# Claude in `/model` (the Claude Code route)

`Claude` in `/model` runs the official Claude Code CLI on the user's own
Claude subscription login (U21). Anthropic refuses subscription OAuth
requests from other harnesses, so Unreal does not call the Messages API for
Claude itself. It uses no MCP server, mod, API key or organization setting.

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

Each Unreal turn runs one Claude Code process. It continues the Session's own
Claude Code conversation: the first turn uses `--session-id`, later turns use
`--resume`.

- **Tools:** Claude Code runs its own tools (Read, Edit, Write, Bash and the
  rest).
- **What Unreal records:** Unreal shows the text as it streams. It records
  every tool call and the result Claude read as a `claude_code` receipt in the
  Session.
- **After switching models:** if another model (such as Codex) answered in the
  meantime, those turns are sent to Claude as context. Codex sees Claude's
  turns the same way.

## Approvals

Claude Code asks Unreal before a call that needs approval. Unreal's permission
mode decides:

| Mode | Edit, Write, Bash and other changes |
|---|---|
| Full Access | allowed |
| Ask | shown to the human in the TUI with the tool and its target, such as `Claude Code Edit: sample.txt` |
| Read Only | denied; Bash is denied before Claude Code's own rules, except commands that only read (docs/permissions.md), which run in every mode |

Claude Code's own rules still apply first:

- **Allowed without asking Unreal:** reads inside the workspace, and commands
  on the organization's allow list.
- **Refused by Claude Code:** commands on the organization's deny list.

## Commands the organization policy refuses (U22)

Some commands are refused by Claude Code's organization policy, such as
`git push`, `curl` or `rm -rf`. Unreal runs such a command itself, in its own
shell:

1. It runs the exact command in Claude Code's working directory.
2. Unreal's mode decides first, as for any Bash call: Full Access runs it, Ask
   asks the human, Read Only denies it.
3. The run is a normal Unreal Bash Operation.
4. Its result goes back to Claude in the same turn.

The organization's deny rules do not apply to Unreal's own shell. This is the
same as for Codex.

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

- `lead`: gpt-6.1-sol at ultra effort. It may start workers itself. As in
  Codex, ultra is never sent as such: requests carry the model's
  `multi_agent_reasoning_effort` from the Codex catalog (xhigh for
  gpt-6.1-sol), and the delegation comes from the lead's prompt and workers.
- `worker-low`, `worker-medium`, `worker-high`, `worker-xhigh`, `worker-max`:
  gpt-6.1-sol at that effort. Workers start nobody, and no worker template runs
  at ultra, so the lead cannot start an ultra child.

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

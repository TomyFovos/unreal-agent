# Unreal Agent fork — contributor notes

This file is read by coding agents (AGENTS.md, and CLAUDE.md via `@AGENTS.md`)
and by people working on this repository.

## Rules

- Add no limits, approvals or stop conditions that the feature does not need.
  Propose them first.
- Run the focused tests for each change, and the whole suite with `-race` at
  milestones. Do not loosen expectations to make a test pass, and do not count
  a skipped test as passed.
- Check a distribution kit on a real install (install, start, one real model
  call) before handing it out.

## Git

- Commit by feature or fix, with its tests. Stage paths explicitly (no
  `git add -A`), and review `git diff --cached` and `git diff --cached --check`.
  Leave out secrets, temporary files and build outputs.
- Push, PR, merge, tag and Release actions need prior user approval for the
  specific commit. A local commit does not authorize them.
- No force push or history rewriting.

## Design to keep

- Host/Session ownership, canonical Operations and receipts, Permission,
  Registry/Translator, the multi-provider runtime, Context Engine, Project
  Instructions, TUI/Orchestration, Markdown Export and the LSP/DAP boundaries.
- No automatic fallback to another provider or to an API key.
- Claude delegates through `~/.local/bin/unreal-agent subagent`. Unreal disables
  Claude Code's Agent, Task, and Monitor in its Sessions. Hand work to a child
  and end the turn to resume on its completion notice. Use `wait` for results
  due within seconds, with at most `--timeout 60s`.

## Build and test

Before every commit, run `make prepush` (use `make prepush BASE=<rev>` to include committed changes since a base).

```bash
make build
make check
go test -race -count=1 -p 1 ./...
git diff --check
```

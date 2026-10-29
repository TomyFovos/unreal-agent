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
  `git add -A`), and check `git diff --cached --check`. Leave out secrets,
  temporary files and build outputs.
- No force push or history rewriting.

## Design to keep

- Host/Session ownership, canonical Operations and receipts, Permission,
  Registry/Translator, the multi-provider runtime, Context Engine, Project
  Instructions, TUI/Orchestration, Markdown Export and the LSP/DAP boundaries.
- No automatic fallback to another provider or to an API key.
- Claude runs parallel work through `~/.local/bin/unreal-agent subagent`, not
  Claude Code's Agent or Monitor: it hands work to a child, ends its turn and
  resumes on the child's end notice. `wait` is only for results due within
  seconds, with at most `--timeout 60s`.

## Build and test

```bash
make build
make check
go test -race -count=1 -p 1 ./...
git diff --check
```

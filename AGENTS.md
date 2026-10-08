# Unreal Agent execution rules

Use native Linux tools in Linux/WSL. Read the current worktree and index before
starting work; preserve existing work and canonical Session history.

## Local checkpoints and remote approval

Create local commits autonomously when an independent feature or bug fix and
its required tests are complete, at stable independently verified checkpoints
during long work, and before moving to another development phase. Individual
user approval is not required for these local commits.

Split commits by feature or responsibility. Stage explicit paths and, when
shared files span features, individual hunks or equivalent partial index blobs.
Never use indiscriminate `git add -A`. Before each commit, review the staged
scope and `git diff --cached`, run relevant tests, require
`git diff --cached --check` to pass, and exclude secrets, temporary files,
generated binaries and unrelated work. Keep incomplete features separate.
Report each local commit SHA and the feature it checkpoints.

GitHub push, PR creation, merge and Release publication require explicit prior
user approval. A local commit does not authorize any of those actions. Stop at
local commits until that approval is provided.

## Protected work and history

Never discard existing work with reset, stash, clean, rebase, amend, force
operations or other history rewriting. Do not delete or overwrite worktrees.
The staged 79-file work in `.handoff/worktrees/prepub-diff-replay` is protected:
do not modify its files/index, unstage it, commit it or remove the worktree.

`AIdea/` is a separate nested repository. Do not stage it in Unreal Agent or
modify it unless the user explicitly assigns work in that repository.

Preserve Host/Session ownership, canonical Operations/Receipts, Permission,
Registry/Translator, multi-provider runtimes, Claude Structured Action Bridge,
Context Engine, Project Instructions, TUI/Orchestration, Markdown Export and
LSP/DAP boundaries. Do not weaken security or silently fall back to another
provider or to side-effecting Claude built-in tools.

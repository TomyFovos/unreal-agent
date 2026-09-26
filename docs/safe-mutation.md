# Safe mutation (Issue #6)

The `harness/mutation` service replaces regular files with an explicit whole-file
SHA-256 revision or `exists:false` creation precondition. A range read must derive
its revision from the same full snapshot. The supported platforms are Linux and
macOS on filesystems implementing advisory flock, same-directory atomic rename
and file/directory fsync. Unsupported syscall semantics fail explicitly. Remote
or distributed filesystems are not a supported deployment.

All cooperating processes must use the same canonical workspace and shared
private StateDir (the default is a per-user cache directory keyed by workspace).
Locks live outside replaced inodes. Relative aliases normalize to one path.
Symlinks in target paths and multiply-linked files are refused. Parents are
opened one component at a time with O_NOFOLLOW and pinned descriptors; replacement
uses renameat relative to that descriptor. Creation requires existing parents.
Workspace mount changes, external directory renames, and hostile filesystem
administration require isolation.

An Authorize callback is mandatory and executes inside the mutation lock.
Host composition supplies the current permission policy; no translator performs
filesystem I/O. Snapshot authorizes read independently.

Mutation v1 requests are executable through the existing RemoteJobHandler
extension. The immutable plan is persisted by Coordinator before Manager dispatch.
Because later Manager checkpoints are not synchronous durable acknowledgements,
Execute writes a private intent proof before side effects and an outcome proof
after completion. A missing outcome is indeterminate and never blindly retried.
The proof is keyed by stable session/operation identity and plan hash. Reusing an
identity with a different plan fails. Proof files are not a scheduler or history;
Session Store remains authoritative for the conversation and Operation state.
Do not delete these proofs until corresponding sessions are permanently retired.

Multi-file requests acquire all locks in lexical canonical order and preflight
all revisions and targets before replacing the first file. Each replacement is
atomic. Later failure/cancellation reports applied, failed, not_applied, or
indeterminate per target; no rollback or crash-atomic transaction is promised.

Direct writes by Bash, editors, debugger targets, and unrelated processes are
outside the cooperating-writer guarantee. The test suite deliberately demonstrates
this boundary. A restricted process policy or isolated workspace is needed for
stronger guarantees.

Issue #5 supplies model-visible native read/write/edit integration evidence.
AST and LSP submit replacements through this API rather than opening write handles.

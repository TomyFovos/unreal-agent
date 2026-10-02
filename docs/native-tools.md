# Native file tools (Issue #5)

The fixed tool registry exposes read, write, edit, grep and glob. The pure
translators in harness/tool/native validate arguments and submit native_file v1
RemoteJob plans. The executor performs all I/O asynchronously. Host composition
creates one mutation.Service with the current permission authorizer and supplies
native.NewHandler(ctx, native.Executor{Files: service}, stableSessionID).

Read returns a whole-file SHA-256 revision computed from the exact snapshot used
for output, even when start_line/lines/max_bytes select only a range. UTF-8 text is
returned; binary files return revision metadata without bytes. Files above 16 MiB
are refused. max_bytes bounds content bytes; JSON metadata/escaping have a separate
finite envelope. No incomplete JSON is decoded as an operation result.

Write requires expected:{exists:false} for creation, or the revision from read.
Edit requires the same revision and exact old_text/new_text; multiple matches
require replace_all:true. Both use the shared mutation service, never direct
writes. Edit binds durable intent to the immutable edit plan before rereading
source, so recovery checks outcome evidence before consulting changed bytes.

Grep uses RE2 regular expressions. Glob paths use /, *, ?, character classes, and
** directory recursion. Traversal is deterministic breadth-first with sorted
entries, bounded to 20,000 entries and at most 1,000 results. Oversized directories
are reported truncated rather than returning an unstable arbitrary subset.
Symlinks are skipped; permission checks run at every directory and file read.
Binary and unreadable grep candidates increment skipped. Cancellation returns a
typed canceled result; truncated explicitly distinguishes partial search results.

Native operations retain typed results in RemoteJob.Handle; TerminalResult is
only a small summary. This avoids corrupting JSON through string truncation.
Unexpected filesystem errors are bounded codes rather than raw sensitive paths.

Tests include real competing processes through native edit, native operation
serialization/Manager integration, durable outcome replay, simulated missing
outcome receipts (combined with real crash-window tests in mutation), whole-file
range revisions, binary/large files, deterministic scoping/output, denied I/O,
and pure translation of nonexistent paths.

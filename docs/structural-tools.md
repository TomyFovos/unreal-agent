# Structural tools (Issue #7)

ast_grep and ast_edit use the official Tree-sitter Go bindings and Go,
JavaScript, and Python grammars, pinned at v0.25.0. These are real syntax trees.
The engine accepts Tree-sitter S-expression queries and requires exactly one
@match node per match.

Example JavaScript query:

```text
((call_expression
  function: (identifier) @callee
  arguments: (arguments) @args) @match
 (#eq? @callee "old"))
```

A replacement of updated${args} changes old(1) to updated(1).
Named captures insert exact source bytes; $$ escapes a dollar. Unknown captures,
unsupported predicates, overlapping ranges, and ambiguous captures are rejected.
Replacement files are parsed again before application. This validates syntax;
type checking and program behavior remain separate work.

Preview is the default. It reports per-file revisions, match counts, byte ranges,
and bounded before/after excerpts. To apply, supply apply:true, expected_matches
from preview, and every target's expected revision. The immutable request binds
a durable mutation receipt before rereading source. Recovery returns saved
outcome evidence or indeterminate, without rebuilding a previous edit from newer
contents.

Every target is prepared before the mutation service acquires sorted locks and
rechecks all revisions and permissions before its first replacement. No-match
targets in a successful batch participate in revision validation and receive an
identical-byte replacement. Per-file atomicity, partial failures, and
noncooperating-writer exclusions match safe-mutation.md. Validation errors do not
start writes. Runtime failures report each applied/failed target without rollback.

Bounds: 128 targets, 2 MiB source/replacement per file, 16 MiB total replacement
payload, 1,000 matches, 16 KiB queries, 32 captures, and 16 query patterns. Output
content defaults to 16 KiB and is capped at 64 KiB plus bounded metadata. Native
parser/query calls time out at 500 ms and conservatively reject runs taking
250 ms or more, preventing acceptance of timed-out partial queries. Cancellation
is checked between calls/matches; native calls can delay cancellation up to their
timeout. Every Parser/Tree/Query/Cursor is closed. The pinned v0.25 callback APIs
retain payload handles, so this integration uses bounded synchronous calls.

Operations use structural_code v1 RemoteJob plans with typed results in Handle.
Translators remain pure. Reads and writes use the existing permission-aware
service; no shell bypass or scheduler is introduced. Handler.Close drains workers
before Session ownership can be released.

## Build

Full AST support requires CGO_ENABLED=1 and a C compiler: GCC on Linux/WSL or
Xcode Command Line Tools on macOS. CI checks the compiler. The Debian build image
enables CGO and the Debian runtime supplies libc. CGO_ENABLED=0 remains buildable;
AST execution then returns unsupported_backend_cgo_required explicitly.
Harbor bundles default to a static CGO build (netgo/osusergo) so native parsers
also work inside older benchmark images. Building a different target architecture
requires a matching C compiler via CC. An explicit CGO_ENABLED=0 creates a
portable bundle with the documented unsupported AST outcome; the bundle manifest
records this choice.

Tests exercise all three real parsers, preview/apply consistency, malformed and
missing syntax, bad replacements/queries, overlap/unknown captures, unsupported
languages/predicates, bounded output, all-target preflight, count/revision
conflicts, real competing processes, durable recovery, denied access and Close.
A late-failure service fixture tests AST outcome propagation; the mutation suite
kills real subprocesses at replacement/crash windows. The benchmark previews
100 JavaScript call rewrites.

References:
- https://github.com/tree-sitter/go-tree-sitter
- https://tree-sitter.github.io/tree-sitter/using-parsers/queries/1-syntax.html

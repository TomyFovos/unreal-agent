# Managed language servers

The `harness/lsp` package implements the LSP 3.17 stdio framing/lifecycle used by
the initial semantic tools. Protocol reference:
[Language Server Protocol](https://microsoft.github.io/language-server-protocol/specifications/lsp/3.17/specification/).

## Host composition

Create one `lsp.Manager` per workspace ownership lifetime, using the same
`mutation.Service` as native editing. Configure each language explicitly with
an absolute server executable, argv, environment and file extensions. Servers
are started lazily and reused for that workspace/language. No executable discovery
or package installation occurs. The mutation authorizer must call the Host's
accepted permission policy; Manager intersects its own policy with each request.

For each Session, attach:

- `lsp.Definition()` and `lsp.Translator{}` to its registry (retain the
  translator for history even when the current tool is hidden).
- `lsp.NewHandler(ctx, manager, sessionID)` to the Operation Manager's
  RemoteJob handlers.
- The handler's `Close` to Session cleanup, before releasing its writer lease.
- The Manager's `Close` to workspace/Host cleanup after borrowers stop.

The handler owns request goroutines; Manager owns processes. Closing one Session
handler does not destroy another Session's workspace resources. Process startup
uses the existing guarded Process primitive. A filesystem/network restricted
process policy fails closed because the harness does not provide an OS sandbox.
Language server processes have the ambient effects that the explicit unrestricted
process grant permits.

## Runner configuration

Set `UNREAL_HARNESS_LSP_SERVERS` to a bounded JSON array, for example:

```json
[{"language":"go","path":"/opt/bin/gopls","arguments":["serve"],"extensions":[".go"]}]
```

The runner adds LSP to the model tool surface only when a server is configured
and LSP is not in disallowed_tools. Its history codec remains available when
disabled. Invalid configuration is rejected before model execution. Executables
are explicit absolute paths; arguments and environment are trusted Host
configuration, never model-selected commands. Raw configuration is not persisted
in session metadata; a digest binds resumed sessions to the accepted configuration.

For a multi-session Host, `agentrunner.NewLanguageTools` exposes `Wrap` and
`Close`: wrap the session tool factory once, then close the owner after Host
sessions stop. Separate services for native and semantic tools share the same
canonical root and durable mutation lock/receipt directory.

## Tool surface

`LSP` accepts a typed request with `language`, `action`, and action-specific
fields. Actions are `definition`, `references`, `diagnostics`,
`document_symbols`, `workspace_symbols`, `hover`, `rename`,
`code_actions`, `apply_code_action`, `restart`, and `shutdown`.

File actions use a workspace path. Positions are zero-based and default to UTF-16;
an explicit `encoding` may be UTF-8, UTF-16 or UTF-32. The client negotiates server
position encoding and converts request positions without allowing a split
Unicode code point. The response reports its encoding and process generation.
LSP-native JSON data remains structurally intact inside the versioned result
envelope; oversized results return `limit` rather than malformed truncated JSON.

Synchronization sends didOpen followed by versioned full-text didChange when the
observed file digest changes. Servers must support open/close and full or
incremental synchronization (a full replacement is valid for incremental sync).
Published diagnostics must include the matching document version; the client
advertises version support and waits within the request deadline. Pull diagnostics
are used when advertised. Unversioned push diagnostics are not presented as
current. Reads changed while a request is in flight return `stale`.

Lifecycle controls require the last observed `generation`. A crash or restart
invalidates it; no persisted PID or old connection is used to control a new
process. Fresh semantic requests may recreate a lost server when no generation
was requested. Close attempts shutdown/exit for idle servers, then cancels and
waits for process cleanup. Requests send cancelRequest on cancellation.

## Safe edits

Before rename or code-action discovery, the client captures a bounded authorized
workspace snapshot and synchronizes configured text documents. Every edit target
must have been observed before the request, except an explicit CreateFile with
an absent precondition. Local workspace file URIs only are accepted. Git metadata,
out-of-workspace paths, overlapping ranges, mismatched document versions,
unknown edit targets and invalid Unicode boundaries fail before mutation.

Workspace edits are converted into one Mutation request:

1. Resolve and validate **every** target and replacement.
2. Pass the original digest/absence preconditions to Mutation.Execute using the
   stable Session/Operation identity.
3. Let the shared mutation service lock and preflight all targets before the first
   write, and report each target's applied/stale/failed/indeterminate result.

CreateFile is supported only for an absent file with an existing parent.
Resource rename/delete, overwrite/ignore-if-exists, confirmation annotations,
server-initiated applyEdit, executeCommand and unresolved code actions are
explicitly unsupported. Code-action discovery returns ephemeral IDs only for
safe edit-only candidates; all others include an unsupported reason. IDs expire
after the next discovery or process generation change. Applying an ID rechecks
the original snapshots through Mutation.

The initial Operation and mutation receipt are durable; process connections and
code-action IDs are not. An interrupted edit Operation already recorded as
awaiting reports `indeterminate` and is not repeated. If a crash happened before
the awaiting checkpoint committed, a read-only lookup of the shared mutation
receipt returns the proven outcome or indeterminate before any semantic replan
or server startup. A second write and a misleading successful no-op are avoided. Successful file mutation followed by failed
server synchronization remains applied and requests resynchronization; it is not
reported as rollback. Atomicity is per file, with Mutation's existing partial
and indeterminate semantics.

## Bounds and unsupported cases

- 4 MiB framed protocol message; 8 KiB header.
- 256 KiB semantic JSON result; oversize returns an explicit limit.
- 2,048 observed/synchronized documents and 16 MiB workspace/synchronized text.
- 128 code-action candidates; only the latest discovery is cached.
- 32 configured language servers; bounded server-request reply queue.
- 64 cached push-diagnostics documents, each within the result limit.
- Configurable request deadline, default 30 seconds.

The initial implementation does not install language servers, run code-action
commands, support notebook/virtual/remote documents, or promise a project-wide
filesystem transaction. Bounded snapshot refusals are explicit; no unchecked edit
fallback is used.

## Verification

Tests spawn a real subprocess speaking Content-Length JSON-RPC and cover
initialize/reuse/close, crash and restart generations, all initial semantic actions,
document version changes, UTF-8/UTF-16/UTF-32 conversion, first-request diagnostics,
cancellation, unavailable/denied process capabilities, bounded results, two-file
rename, all-target stale preflight, CreateFile, safe and rejected code actions,
expired action IDs, pure translation, durable handler results and interrupted
mutation recovery. Mutation tests cover process lock/crash/partial-write behavior;
LSP additionally checks that detailed target outcomes survive result encoding.
This protocol fixture does not claim verification against every third-party
language server.

An interrupted awaiting rename/code-action consults the shared Mutation receipt
under the current Host permission policy. A proven result is returned without
starting the language server or replaying edits. Missing evidence stays
indeterminate; receipt lookup is not permission to repeat a side effect.

Awaiting apply_code_action receipt recovery has a dedicated regression using a
lost previous-generation action ID: applied receipt, missing receipt
(indeterminate without replay), and current permission denial. It uses the
durable Mutation receipt before consulting any transient action/server state.
This fixture coverage does not validate a production language server.

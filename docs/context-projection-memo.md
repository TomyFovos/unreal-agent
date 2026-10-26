# Build-local projection costs

Context selection uses the provider's pinned, pure final serializer. Marginal
candidate sizing formerly rebound and measured the same public call/receipt
many times during required selection, group completion and diagnostics.

`contextbuilder.buildProjected` now owns a disposable memo of successful
candidate costs and successful public-partner baseline measurements. It is
created for each Build and is never retained in a Host, Session or generation.

The key uses the Engine's unique immutable Unit identity, typed history source,
kind, class, group, origin, range and candidate attributes, selection reason,
ReferenceOnly and the request-local whole-group portable-retry flag. The partner
key includes the actual eligible public ToolCall, not just its CallID. Engine
copies with modified Required/Class attributes are distinct. Running and final
updates receive distinct Unit identities; a later Build always measures again.
Public body hashes do not establish provenance or authorize a cache hit.

Memory is bounded to 16 successful representations per visited Unit and the
Build's fixed public partners plus the empty baseline. Excess representations
are recomputed. Errors and invalid marginal values are not cached. Native
replay, schemas, model and receipt identity remain fixed inside this Build.

Base instructions/state, the complete selected projection, the final renderer,
final input budget, continuation matching and typed receipt-provenance checks
still execute. A candidate cost cannot bypass these checks. This memo changes
no canonical receipt, acknowledgement, Permission, Operation, tool execution,
Action replay, authentication, timeout or provider selection.

## Verification and remaining gate

The original Go 1.27.1 / JSON v2 / race implementation is compared through an
explicit compiler overlay of cd5f3c6's `compaction.go`. Synthetic cases cover
nine default tools, multiple reads, past layers, running/final updates,
completed/failed secret stubs, native-to-portable retry and typed failures.
Compare selected Units/order/representation, the complete request and validated
typed receipt origins, estimates/reserves/budget/Utilization and diagnostics
(excluding retrieval elapsed time). The reference never imports a compatibility
shim. CPU profiling and interval instrumentation run separately from gates.

The scoped follow-up reduced a representative Build from 27 Measure calls to
8 with identical semantic output. At the bd28f1b checkpoint, the original
`TestClaudeStructuredHostFailFixPassCanonicalOperationsAndRestart` still timed
out in its unmodified five-second race condition. Its 13 Operations, 15 fake
generations, replay and restart/resume checks remain intact. This optimization
does **not** establish that the required race gate is fixed or that the
distribution candidate is accepted.

The subsequent bd28f1b design review authorized a paired observer diagnostic.
See [interactive test observation](interactive-test-observation.md) for the
bounded comparison and test-only correction. The profile's attribution to
Host Inspect's JSON clone under the Session lock did not by itself establish
the timeout's cause. Neither change extends deadlines, shortens the fixture,
retries until passing, or redesigns the production Host.

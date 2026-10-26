# Interactive integration test observation

The bd28f1b design review isolated the Structured Bridge integration test's
observer before authorizing any further production optimization. The test had
called `Inspect(after=0, limit=256)` repeatedly after 1ms sleeps. Every call
cloned the growing public history and operation snapshot under the Session
lock, then encoded and decoded that full snapshot through the Gateway.

## Paired diagnostic

Two compiler overlays used the same source, native Go 1.27.1, `-race`, default
GORACE/GOFLAGS, CGO enabled, compiler cache, and isolated synthetic HOME/state.
The same test retained its five-second interaction deadline, 13 Operations,
15 fake generations, original payloads, permissions, context/schema budget,
Final, duplicate replay and restart/resume assertions. A preserved the 1ms
full-view poll; B used production Gateway Subscribe hints, incremental Inspect
pages, full resynchronization after gaps, and a full final Inspect. Timing and
counter instrumentation was identical in both variants and stayed outside the
source checkout. Both binaries were compiled before the sequential comparisons.

The first pair both passed. Two additional pairs were run to check the already
observed variability; all three pairs, including failures, are retained:

| Variant/run | Result | Inspect calls | History items transferred | Session lock held by Inspect (seconds) |
| --- | --- | ---: | ---: | ---: |
| A/1 | PASS | 117 | 3005 | 2.846 |
| B/1 | PASS | 130 | 235 | 0.459 |
| A/2 | five-second timeout | 93 | 1269 | 2.408 |
| B/2 | PASS, including one resync | 127 | 234 | 0.457 |
| A/3 | five-second timeout | 104 | 2005 | 2.396 |
| B/3 | PASS | 130 | 235 | 0.406 |

Counters cover the observed fixture, including lifecycle observations when it
finishes. Failed runs stop earlier and are not equivalent complete workloads.
Intervals overlap execution; neither sums nor overall test durations describe
the interaction deadline or an additive CPU speedup. The comparison supports
observer-induced load/lock interference; it does not prove a sole production
latency cause or solve historical live-provider/SSE issues.

## Test-only correction

Only `testClaudeBridgeHost` uses the event-driven test observer. Other tests
retain full Inspect coverage. The production Host, Gateway, TUI, providers,
context budget and projection memo are unchanged.

The observer keeps one five-second context across subscriptions/resyncs.
Notifications are wakeups only: no event Item/Operation payload satisfies the
predicate. It obtains canonical cursor pages via Inspect, coalesces queued
hints, rejects regressed generation/revision/cursors, fully resynchronizes after
gap/disconnect/closure, and rechecks a fresh full Inspect before returning a
completed view. Subscription cancellation runs on success, error and timeout.

Unit fixtures cover misleading events, pagination, final verification, gap,
disconnect, EOF, timeout/cancel, inspection failure and snapshot regressions.
`TestConcurrentInspectSubscribeCanonicalConsistency` separately overlaps full
Inspect with 24 durable Operation publications, verifies ordered untorn history,
monotonic snapshots, post-commit notifications, isolated returned values,
idempotent input receipts and equality with the synthetic durable Store. The
existing snapshot barrier, slow-subscriber and Gateway process tests remain.

Acceptance still requires the exact standalone race test, complete native race
suite, Structured Bridge fake gate, check/build, Python installer/wrapper tests
and packaged-binary checks. No real Session, inference, Sandbox or publication
is part of these tests.

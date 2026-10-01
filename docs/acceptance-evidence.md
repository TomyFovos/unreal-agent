# Issue #3 acceptance evidence

This records evidence, not an assertion that a passing CI run proves every AC.
Test names below are in `harness/subagent` unless another package is named.
No live model is used by these lifecycle fixtures.

## Initial boundary audit

`existing` means sufficient focused evidence already existed; `partial` means
related evidence existed but the requested persistence/crash boundary was not
fully covered; `missing` means no dedicated evidence was found.

| Boundary | Before / after at audit | Existing evidence | Added evidence |
|---|---|---|---|
| Outgoing intent commit | partial / partial | Coordinator durable dispatch, cancellation storage-failure tests | `TestSubagentMessageCommittedPrefixRecovery`, `TestChildToParentMessageCommittedPrefixRecovery` |
| Send | partial / partial | `TestLostAckRetryRejectsChangedPayload` | `TestTransportDisconnectAtSendAppendAndACK` and message prefix recovery |
| Receiver AppendInput commit | partial / existing | Host `TestPeerCanonicalProvenanceDedupAndResume`, persistence-failure/no-ACK tests | Transport disconnect and both message directions |
| ACK | partial / partial | `TestChannelBindingAndAckAfterCommit`, lost-ACK retry | Transport disconnect before/after ACK, fresh endpoints and resumed receiver |
| Spawn | existing (cancel-before-spawn) / partial | `TestCancellationBeforeHandlerRegistration`, parent-process crash recovery | `TestSubagentSpawnCreateReadyCommittedPrefixRecovery` |
| Child create / ownership | partial / partial | Host process-death writer-lock test; safe create/configuration tests | Spawn/create prefix recovery, `TestSimultaneousChildResumeUsesProcessSharedOwnership` |
| Ready configuration commit, send, receiver commit, ACK | partial / partial | Stable ready dedup after parent crash | Spawn/create/ready prefix recovery plus transport disconnect |
| Child Finish commit | missing / existing | `TestCommittedFinishCollectedWithoutRespawn` | `TestChildFinishBeforeCommitRecovery` |
| Parent terminal Operation commit | existing / existing | `TestCommittedFinishCollectedWithoutRespawn`; Coordinator `TestCoordinatorResumesSavedTerminalOperation` (completed/failed/canceled) | No redundant test added |

Prefix recovery restores exact complete records emitted by real Hosts and actual
subprocess IPC. It discards all volatile endpoint and process state. Sending and
ACK delivery have no independent persistent record: their before/after durable
states are deliberately covered together, with separate transport cut tests.
The ready intent is the stable ReadyID in canonical ChildConfig, not a second
outgoing-intent log. Partial disk-tail recovery is already covered by localfile.

## Additional required integration evidence

| Property | Evidence |
|---|---|
| Simultaneous resume of the same child, exactly one writer, typed loser, same ID | Two independent OS processes call Host.Resume concurrently; winner retains the actual flock until both results are checked |
| Owner death, IPC EOF, surviving descendant inheriting stdin/stdout/stderr | `TestParentDeathEOFCleanupWithSurvivingDescriptorDescendant`; escaped descendant stays alive while the child runtime closes and releases ownership within three seconds |
| Ordinary process-group descendant termination and inherited output-pipe drainage | Existing primitives `TestProcessExitTerminatesDescendantsWithoutWaitingForInheritedOutputPipes`, `TestCancelProcessTerminatesDescendants` |
| Side effect followed by owner death before terminal persistence | `TestInterruptedChildShellRecoveryDoesNotReplaySideEffect`; real shell, frozen/killed owners, same child ID; recorded-start yields interrupted and missing-start yields unknown; no result/Finish fabricated |
| Persisted cancel before canceling checkpoint and after it | `TestPersistedCancelChildRecoveryNeverRespawns`; child log unchanged, no writer, explicit resume rejected |
| Cancel/Finish race and terminal immutability | Existing Coordinator `TestAcceptedCancelBeatsLateCompletionAndTerminalIsImmutable` |
| Header/configuration crash recovery remains fail closed | Spawn/create prefix cases and `TestUnconfiguredChildWithCanonicalInputFailsClosed` |

The shell integration injects a fixture-only runtime with explicit shell
permission to exercise recovery of an accepted shell operation. Production
ChildConfig still rejects ambient shell/process capabilities. It does not prove
that production bounded templates can launch arbitrary shell commands.

Existing integration tests already cover parallel separate children, ready-driven
independent parent work, question/idle/reply/Finish, structured result ownership,
permission narrowing, fork ownership, and bounded malformed frames. Those tests
were retained rather than reimplemented.

## Production correction found by these tests

Manager recovery previously rejected a child whose header was committed before
its configuration. It now permits only an empty header or a matching sole
instruction binding to reach Host.Open's process-shared ownership gate. Missing
lineage with inputs, operations, Finish, or mismatched bindings remains rejected.

## Limits

Commit-prefix fixtures represent crash states at complete committed-record
boundaries; they do not kill processes inside individual filesystem syscalls.
Hardware/power-loss fault injection and production provider behavior are outside
this evidence. Actual parent death, lock contention, EOF and interrupted shell
effects are exercised by dedicated subprocess fixtures above.

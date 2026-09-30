# Async child agents

## Composition

The parent Host runtime factory gets its owner with host.SessionFromContext(ctx).
Build subagent.NewManager(ctx, Config{Owner: owner, Directory: sessionDirectory,
Binary: absoluteAgentBinary, Arguments: []string{"child", "--stdio"},
Templates: allowedTemplates}), add it to the existing LocalOperationManager,
and compose tool/subagent.Extensions(owner.ID, templates, false) with
tool.WithExtensions. Runtime.Close must call the handler's Close.

Binary and arguments are trusted Host configuration. A model can select only
an explicitly registered template and supply a task. Templates fix workspace,
resolved provider/profile versions, non-secret runtime references, and child
capabilities. The parent process permission must explicitly allow unrestricted
process launch. Children use bounded file/network executors and cannot launch
arbitrary processes. The child tool capabilities must be a subset of the
parent's current policy. No credentials are serialized or passed as plan data.

The same binary's child --stdio command calls
subagent.Serve(ctx, os.Stdin, os.Stdout, childFactory).

The factory receives ChildConfig and a Sender. It creates a permission policy,
a child Host, and a runtime using a handler configured with Child: &config and
SendParent: sender. Child extensions advertise SendParent and Finish only.
Open the Host with ID=config.ChildID, Lifecycle="child", Heartbeat=0,
Configuration=json.Marshal(config), and Initial=[]inbox.Input{config.InitialInput()}.
Return the child Session and an io.Closer which closes its Host then its policy.
Return writer-lock conflicts unchanged; Serve retries ownership handover for up
to ten seconds. Any partial factory failure must close the partial Host/policy.

The factory must resolve config.Runtime into the recorded provider/profile
selection. It must not mutate process environment, run another Coordinator
outside this Host, broaden config.Policy, or use stdout for diagnostics.

## Persistence and ownership

A Start RemoteJobPlan is committed before launch. Child ID is deterministically
derived from parent session ID and public Operation ID. Resuming the parent
opens the same child, under the existing session writer lock. A persisted Finish
is collected without running that child's model again. Unknown shell outcomes
retain the existing interrupted-operation behavior.

The wire format is versioned JSON prefixed by a four-byte big-endian byte count,
bounded to 256 KiB. Each OS pipe has one unpredictable connection identity and
fixed sender/target IDs. Writes are serialized through the existing Process
primitive and bounded input/ACK queues. EOF closes the child's Host and releases
its writer lock. Cancellation first requests a normal hard stop, then terminates
the process if it does not stop within two seconds; the Process primitive owns
the final process-group escalation and descendant cleanup.

Peer messages are canonical InputPeer records. They are rendered with explicit
machine provenance. Direct Session.Submit cannot forge a Peer; the channel
uses SubmitPeer and validates sender/target/handle. An ACK is sent only after
AppendInput commits, and carries its receipt. Retries with the same immutable
payload return the original receipt; changed payloads return a conflict.

SendParent and SubagentSend are RemoteJobs, so outgoing intent precedes effect.
The stable ready identity lives in ChildConfig and can be retried on reconnect.
Ready and progress messages are independent of the Start operation's terminal
result. A final report travels only through a canonical HostRecord(kind=finish)
and the parent operation's typed Handle. It is never duplicated as a Peer.

Finish is rejected while other calls or operations are pending. The canonical
Finish record precedes the owner's pure decision to stop before another model
turn. Ending an ordinary child turn means waiting, not completion.

Cancellation is a persisted control Input, followed by a saved canceling
checkpoint, followed by Manager.Cancel. The parent Coordinator serializes the
outcome: committed terminal results are immutable; accepted cancellation wins
over late worker completion. Recovery applies a committed cancel before dispatch.
Forked sessions clear inherited operation ownership and cannot control the
original parent's children.

## Operator APIs

ReadChild returns a bounded canonical history page, operations, ChildConfig,
and Finish. Empty generation means runtime ownership is unknown.

SteerChild, CancelChild, and ResumeChild validate parent generation and the exact
owned Operation/child pair. SteerChild records a Send RemoteJobPlan inside a
durable host operation-intent Input; it does not write directly to a pipe.
Caller-stable InputID provides conflict detection and retry receipts.

Live parents automatically recover nonterminal children when their own Host is
resumed. ResumeChild validates that ownership; it does not create another
executor. Terminal children cannot be resumed, and a stopped parent must first
be resumed through its Host owner API.

## Verification

Tests use actual child and parent subprocesses, including forced parent death,
bidirectional Inbox messages, ready deduplication, durable steering retries,
Finish, cancellation, and child crashes. Unit tests cover framing bounds and
partial/coalesced frames, channel identity, commit-before-ACK, peer provenance,
cancellation arbitration, and Finish eligibility. Production provider/CLI
composition is supplied by the command package; this package imports no command.

## CLI templates and project instructions

The unreal-agent serve configuration accepts a Subagents map. Each entry
contains a Runtime identity and bounded Permissions; the model chooses the
entry's name, never binary arguments, credential material, or capabilities.
The CLI composes subagent handlers into the same RuntimeFactory as native tools,
AST, LSP and DAP. An empty template map preserves the existing parent runtime.
Children have native/AST executors with bounded file access. External LSP/DAP
resources and arbitrary child subprocesses require a sandbox and are rejected
by the current bounded child policy.

At spawn the parent Host supplies its canonical bound ProjectInstructionSnapshot
as ChildConfig.ProjectInstructions. The child's Host binds that configuration
atomically at Create and replays it on Resume. The delegated Peer task contains
only task text, and parent conversation is never copied. An explicit none
snapshot remains none after a new AGENTS.md appears on disk. Omitted snapshots
in legacy configurations mean none and never trigger child discovery.

The parent resolves each child's shared MutationStateDirectory using the same
canonical workspace/cache rule as native and LSP tools. It is a nonsecret
absolute lock/receipt directory recorded in the template/configuration and
passed directly to the child executor. Child environment is explicitly limited
to PATH and LANG: HOME, provider keys, proxy credentials and parent environment
are not inherited. This keeps mutation locking shared without ambient home
configuration. Changing a recorded template/configuration fails recovery
explicitly, rather than replaying under a new runtime.

CLI integration tests build and launch the actual serve and child --stdio
commands against an HTTP fixture. They assert revision A after disk becomes B,
inheritance of none, independent child transcript, one canonical binding/task/
ready record, and Finish through the Operation result path. Three simultaneous
real child processes also prove that C completes and wakes its parent while
A/B remain idle.

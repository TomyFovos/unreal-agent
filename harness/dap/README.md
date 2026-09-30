# DAP runtime

Host composition creates dap.NewManager with an explicit permission context and
fixed AdapterConfig entries. Register it as an operation.RemoteJobHandler, the
tool/dap Definition and translator New(manager.Generation()), and call Close when
the Host releases resources. The manager uses the existing Process primitive,
not an additional agent loop or task database. No I/O occurs in translation.

## Runner configuration

Set `UNREAL_HARNESS_DAP_ADAPTERS` to a JSON array (64 KiB limit), for example:

```json
[{"id":"lldb","path":"/opt/bin/lldb-dap","directory":"/workspace","arguments":[],"allowed_attach_pids":[]}]
```

Adapter installation and platform-specific options are caller responsibilities.
Launch requests supply typed program/argv/cwd; attach additionally needs the PID
listed in the trusted configuration. `launch_fields` and `attach_fields` hold
adapter-specific options and are not accepted from model requests. Raw adapter
configuration is not persisted in session metadata; only its digest binds resume.
DAP is advertised only when configured and not in disallowed_tools. Its history
codec remains available while disabled.

`agentrunner.DebugTools` wraps a ToolFactory, creating an isolated debugger
owner/generation for each Host-owned Session runtime and closing it with that
runtime. Parent cancellation and explicit Close use the same drain path; racing
callers wait for cleanup before the ownership lease can be released.

## Identity, recovery and operations

The durable typed plan contains a stable logical session ID and the current
Host owner generation. The returned Handle combines that ID with generation.
On restart every previous-generation plan (including Ready checkpoints) becomes
typed expired. No automatic relaunch, attach, evaluation, stepping or replay is
performed. Creating a new debugging session requires an explicit new request.
Successful results use versioned RemoteJobState.Handle so generic text
truncation cannot corrupt the canonical result.

Commands: launch, attach, setBreakpoints, threads, stackTrace, scopes, variables,
evaluate, continue, pause, next, stepIn, stepOut, inspect, disconnect.
Frame/variable references include stop_epoch and are rejected after execution
continues. Adapter events update live stopped/running/terminated state. Inspector
state is derived from this resource; no separate durable debugger database.

Each request is bounded to 30 seconds; eight logical session IDs per owner,
100 result items, 2048 bytes per text field, 32 KiB result, 1 MiB protocol frame.
Overlarge structured results fail explicitly. Adapter stderr/output events are
drained but not copied to canonical history. The adapter-specific launch/attach
configuration is trusted Host configuration; model arguments stay typed.

## Protocol and ownership

Single-session stdio DAP uses Content-Length framing and initialize -> pending
launch/attach -> initialized -> configurationDone -> completed start. Reverse
runInTerminal/startDebugging requests are unsupported and expire the adapter.
No additional process or debugger owner is created.

Process permission must be enforceable for adapter and debuggee. This initial
backend requires explicit unrestricted filesystem/network/process permission;
requested sandbox constraints fail closed. Attach additionally needs a Host
PID allowlist. Evaluation additionally requires the DAP.Evaluate tool capability.

Normal Close requests disconnect with terminateDebuggee=true for owned launch
sessions and false for attached targets. Explicit termination of an attached
target is rejected. Cancellation, pipe/protocol failure and parent death expire
the resource and terminate the owned adapter process group; no signal is sent
directly to an unrelated attached PID. Adapter-specific detach-on-crash behavior
still belongs to that adapter. An attach PID is not a durable identity.
Cancellation performs a bounded disconnect attempt before adapter termination.
A process crash cannot guarantee that an adapter detaches cleanly.

## Tests

A real subprocess speaks the DAP wire protocol (deterministic test adapter).
Coverage includes all initial commands, setup ordering, invalid framing, bounded
UTF-8 output, attach/terminate ownership, adapter exits, debuggee terminated
events, cancel-before-dispatch and during evaluation, generation expiration,
stopped-reference expiry, denied/sandbox-unavailable/evaluation permissions, and
pure translation. No production debugger binary was installed in the review
environment; adapter-specific smoke tests remain opt-in integration validation.

Protocol source: https://microsoft.github.io/debug-adapter-protocol/overview

Owner cancellation and Close cancel active requests before graceful cleanup. Those
requests release the command lock while the adapter remains alive for a bounded
disconnect; attach uses terminateDebuggee=false. Ordinary request cancellation,
protocol failure, and the final cleanup deadline still expire/kill the adapter.

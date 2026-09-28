# Interactive terminal and local Host

`make build` now produces `bin/unreal-agent` in addition to the compatible one-shot runner and authentication CLI. Linux and macOS are supported; on Windows use WSL. The terminal needs a controlling TTY. The Unix socket is local only; remote TCP/browser access is not provided.

## Start an owner, then attach

Create a nonsecret JSON configuration (replace paths and the explicitly installed model ID):

```json
{
  "Runtime": {
    "Version": 1,
    "Provider": {
      "version": 1,
      "provider": "ollama",
      "model": {"id": "your-installed-model", "family": "custom", "capabilities": ["tools"]},
      "endpoint": "http://127.0.0.1:11434/v1",
      "auth": {"method": "none"},
      "max_attempts": 2,
      "source": "operator configuration"
    },
    "Profile": {"id": "conservative", "version": 1, "source": "operator configuration"},
    "Workspace": "/absolute/workspace",
    "SystemPrompt": "Help with this workspace. Inspect current files before editing.",
    "ReasoningEffort": "low",
    "DisallowedTools": ["Bash"]
  },
  "Permissions": {
    "Tools": ["read", "write", "edit", "grep", "glob", "ViewImage", "SkillUse"],
    "ReadRoots": ["/absolute/workspace"],
    "WriteRoots": ["/absolute/workspace"],
    "NetworkOrigins": ["http://127.0.0.1:11434"]
  }
}
```

The explicit model entry is the operator's catalog choice, not an automatically discovered model. Provider/profile validation still applies. Omitting permissions denies access. Bash requires the existing unrestricted process permission contract; it does not inherit a filesystem-only sandbox. Native mutation retains its documented local-filesystem constraints; use a Linux filesystem inside WSL for writable workspaces.

Run the owner in one terminal (use short absolute paths for Unix sockets, at most 100 bytes):

```sh
bin/unreal-agent serve --config /absolute/runtime.json   --session-directory /absolute/state/sessions   --socket /absolute/private-runtime/host.sock
```

The socket parent directory must be owned by the current user with mode 0700. A new directory is created privately; the socket and ownership lock use 0600. Lock contention fails explicitly, and only a stale socket protected by the acquired lock can be removed. Access to this directory authorizes local frontend access to the owner's sessions and credential UI; it is not a multi-user service.

In another terminal:

```sh
bin/unreal-agent attach --socket /absolute/private-runtime/host.sock --mode create --session work-1
bin/unreal-agent attach --socket /absolute/private-runtime/host.sock --session work-1
```

Each client connects to the existing owner. Closing/detaching a client leaves execution running. Ctrl-C or SIGTERM in **the serve process** stops that owner and drains its session runtimes. Restart the owner with the same configuration and resume saved state:

```sh
bin/unreal-agent attach --socket /absolute/private-runtime/host.sock --mode resume --session work-1
```

Attach does not implicitly create a writer. Resume while a session is already active fails with `writer_owned`. Runtime identity includes provider/model/profile, workspace, system prompt, reasoning effort and disabled tools; changing it for an existing interactive session is rejected. The older one-shot runner retains its own existing configuration contract.

## Terminal controls

- Enter submits another prompt to the same session; left/right, home/end and backspace edit grapheme clusters. Bracketed paste is input text and cannot execute slash commands or submit a pasted newline.
- Ctrl-D or `/detach` disconnects. `/stop` and Ctrl-C submit a normal durable hard-stop control; `/stop idle` requests stop when idle. `/resume` opens a new ownership generation after stopping.
- `/retry` resends the most recent uncertain input with its original input ID. It cannot create duplicate canonical input after an ACK was lost. A pending request can be canceled with Ctrl-C; cancellation of waiting alone does not retract an already committed input.
- `/help` lists commands. Operation status and errors are projected from the owner. The display retains the most recent 1,024 transcript entries, clips each displayed entry to 4 KiB, and leaves full canonical history available through paged Inspect. This first terminal has no scrollback/history browser.
- Resize is detected without changing session state. Output is bounded to terminal rows/cells, and provider/tool text cannot inject terminal escapes. Native terminal writes observe cancellation and a two-second deadline. Rendering is independent of the subscription receiver.

## Private authentication

To use managed credentials, pass `--credential-directory /absolute/private-credentials` to `serve`. For OpenAI select `provider: openai`, a supported explicit model/family, the OpenAI endpoint/network origin, and `auth: {"provider":"openai","method":"api_key","id":"primary"}` in the runtime configuration. No key belongs in this file.

In the TUI use `/login openai primary`, then enter the key at the masked prompt. Ctrl-C cancels private entry. `/logout openai primary`, `/credentials` and `/methods` use the same UI-neutral authentication service as the authentication CLI. The support matrix explicitly reports supported/configuration-only, unsupported and deferred flows. OAuth/browser login is not implemented. Logging out removes managed credentials; external source ownership is unchanged.

Keys pass over the private local socket to `authflow.Service`, never as Inbox input, tool arguments, Session history, screen text or command history. Secret buffers are cleared where mutable; Go strings and transport buffers are transient memory, not a promise of forensic memory erasure. Authentication methods are validated before private entry. A stored key is checked by the next real provider request, not by a speculative login-time inference call.

## Projection and progress contract

The wire protocol is versioned `/v1/...` HTTP over the private socket. Submit returns the existing durable Host receipt and checks generation. Requests are capped at 128 KiB; pages at 256 items; frames at 32 MiB; subscriptions at 128 queued events. An oversized canonical item/frame produces an explicit error rather than truncated/invalid JSON. The server uses a write deadline and the Host's existing bounded subscription/gap mechanism. A slow client owns no execution lock.

The receiver subscribes atomically, pages canonical history by sequence, and replaces operation snapshots with the latest owner view. Duplicate sequences/revisions are ignored. Gaps, changed generation and connection loss trigger a visible resync; disconnected runtime status is unknown. No frontend state is written to disk. Reconnect retries every 250 ms and stops on client cancellation.

`llm.RequestOptions.Progress` is optional. The Responses adapter emits only text deltas and attempt-reset markers. Host progress is ephemeral, keyed by generation, turn, model-call epoch and transport attempt, capped at 64 KiB and emitted at most about 33 times/second plus reset/completion markers. Late/canceled calls cannot overwrite newer progress. Completed model responses alone enter canonical history; canonical completion replaces temporary text. Adapters without this callback show an explicit completed-response waiting state.

`agentrunner.NewRuntimeFactory` creates fresh per-session components; `RuntimeConfig.NewTools` lets composition inject protocol/subagent handlers with the same owner context. Shared protocol managers belong to the Host composition and are closed after `Host.Close`; each session closes only its own handlers. `tui.Config.Panel`, `Command`, and `Updates` plus the bounded gateway extension endpoint support a child viewer without making the base TUI depend on subagents.

## Validation

`make test check build` runs race tests, vet, formatting and builds. Added tests cover real subprocess attach/detach, durable retry, multiple prompts, writer contention, resume generations, slow subscribers, bounded transient progress, real SSE retry/reset, private authentication, gap rebuild, Unicode/paste/resize, and cancellation. Linux additionally runs a real PTY subprocess with Unicode input, resize, masked login and detach. The Darwin build is checked with `GOOS=darwin CGO_ENABLED=0 go build ./cmd/unreal-agent` on this pre-AST branch; final AST composition has its separate CGO requirement.

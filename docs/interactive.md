# Interactive terminal and local Host

`make build` produces `bin/unreal` (an alias of the same `bin/unreal-agent`
binary), the compatible one-shot runner and authentication CLI. Linux and macOS
are supported; on Windows use WSL. The terminal needs a controlling TTY. The
Unix socket is local only; remote TCP/browser access is not provided.

## Normal startup

Use one command:

```sh
unreal
unreal test
unreal backend-fix
```

No name selects `default`; a positional name selects that session. Names use
ASCII letters, digits and dashes, are at most 128 bytes, and cannot begin with
a dash. `unreal -test` is an unknown option. `unreal --help` lists the supported
options; options can appear before or after the positional name.

The launcher probes the configured gateway using its existing read-only
authentication-methods request. A healthy Host is reused. Otherwise it starts
the same executable's `serve` subprocess in a separate OS session, with stdin
from `/dev/null` and stdout/stderr in a private log, and waits for a real gateway
response. The default readiness deadline is 20 seconds; use
`--startup-timeout 2m` for a slower configured runtime. Readiness is not inferred
from a sleep or the presence of a socket file.

The launcher attaches to an already running session, resumes a stopped one,
and uses the existing atomic create-or-resume operation when the Host has no
in-memory view. This also recovers saved sessions after a Host restart. A
concurrent opener that sees `writer_owned` rechecks the current Host's view and
attaches when that owner is already running it. A writer held elsewhere produces
an explanation such as `session "test" is currently owned by another writer`;
its existing typed cause is retained.

| Resource | Default |
| --- | --- |
| Runtime/policy | `~/.config/unreal-agent/runtime.json` |
| State | `~/.local/state/unreal-agent/` |
| Canonical sessions | `STATE/sessions/` |
| Socket | `$XDG_RUNTIME_DIR/unreal-agent/host.sock`, otherwise `STATE/run/host.sock` |
| Host log | `STATE/host.log` |
| External Codex auth | `~/.codex/auth.json`, with existing Codex discovery overrides |

`XDG_CONFIG_HOME` and `XDG_STATE_HOME` override the corresponding home defaults.
An existing `XDG_RUNTIME_DIR` must belong to the current user with mode 0700;
an unset or nonexistent runtime directory uses the state-directory fallback.
Private state/socket directories are created with 0700 and checked, without
loosening existing permissions. Socket, gateway lock, launcher startup lock and
Host log are 0600. Symlink/hardlink or nonregular startup-lock/log files are
rejected. Launchers coordinate with `host.sock.start.lock`; the existing
`host.sock.lock` is the authoritative serve ownership lock. Only serve, after
acquiring that lock, may remove a stale socket. Lock files are never unlinked.

Options `--config`, `--state-directory`, `--session-directory`, `--socket`,
`--codex-auth-file` and `--credential-directory` override defaults. An already
healthy Host retains the configuration it started with; launcher options cannot
replace its permission policy or runtime. To change that configuration, stop
the owning serve process and restart it. Existing session runtime identities
still have to match on resume. Old sessions in a different low-level store can
be selected with `--session-directory` when starting a Host for that store.

If the normal configuration is absent, the launcher discovers signed-in external
providers and their selectable models without inference. It asks once for the
initial provider when both are available, then atomically creates a nonsecret
`runtime.json` with permissions 0600 in a private 0700 directory. The initial
policy permits native read/search of the current workspace; it grants no shell,
write, unrestricted filesystem or unrestricted network access. It never replaces
an existing file. Missing explicit `--config` and malformed configurations still
fail with setup/typed error guidance.

Bootstrap grants no filesystem tools when the initial workspace overlaps known
private provider/Host storage or contains the user's home directory. Text startup
still works; normal project directories receive the bounded read/search grant.

Normal startup also discovers additional providers alongside an existing
`Runtime.Provider`, without changing that initial runtime or permission policy.
Claude Team/Enterprise still requires explicit managed-policy trust. Bridge
registration checks exact effective Unreal MCP grants; a blocked bridge leaves
Claude available for text requests and reports a safe capability status.
See [normal launcher configuration](normal-launcher.md) for this one-time opt-in,
discovery controls and conservative defaults.

The existing ExternalCodex resolver validates discovery and rereads credentials
on requests; tokens are never imported or written into configuration. Claude CLI
owns its authentication. Inline Codex access tokens/account IDs are omitted from
the background Host environment. There is no provider/API-key fallback, automatic
login, install, refresh or credential overwrite.

`/detach` and Ctrl-D leave the Host and session running; `unreal test` returns to
the same conversation. Ctrl-C keeps the TUI's existing session-stop behavior.
The background Host remains alive if the launcher exits or its startup deadline
expires after a process was started; a later invocation probes/reuses that Host.
The log is append-only and has no automatic rotation. All Live Dock controls,
input safety and rendering stay the same.

## Low-level commands: start an owner, then attach

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

Attach does not implicitly create a writer. Resume while a session is already active fails with `writer_owned`. The immutable creation identity includes provider/model/profile, workspace, system prompt, reasoning effort and disabled tools; changing that startup configuration for an existing interactive session is rejected. `/model` records a separate versioned model/effort selection, described below, without replacing the creation identity. The older one-shot runner retains its own existing configuration contract.

## Use existing Codex CLI / ChatGPT credentials

The interactive Host can use the existing `openai-codex` provider with a
ChatGPT login owned by Codex CLI. First sign in through Codex CLI with
`codex login`, if needed, and confirm that `~/.codex/auth.json` exists as a
regular private file (mode 0600). Keep that file outside the workspace's tool
read roots. Codex CLI must use file-backed credential storage; an OS keyring
alone is not an auth file. See [OpenAI's authentication documentation](https://learn.chatgpt.com/docs/auth).

Create this nonsecret runtime JSON, replacing `/absolute/workspace` with your
workspace. This example explicitly selects
[GPT-6.1 Sol](https://developers.openai.com/api/docs/models/gpt-6.1-sol):

```json
{
  "Runtime": {
    "Version": 1,
    "Provider": {
      "version": 1,
      "provider": "openai-codex",
      "model": {"id": "gpt-6.1-sol", "family": "openai-reasoning", "capabilities": ["tools", "reasoning"]},
      "endpoint": "https://chatgpt.com/backend-api/codex",
      "auth": {"provider": "openai-codex", "method": "oauth", "id": "external-codex"},
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
    "Tools": ["read", "grep", "glob"],
    "ReadRoots": ["/absolute/workspace"],
    "NetworkOrigins": ["https://chatgpt.com"]
  }
}
```

Start the Host in Terminal 1:

```sh
bin/unreal-agent serve --config /absolute/codex-runtime.json \
  --session-directory /absolute/state/codex-sessions \
  --socket /absolute/private-runtime/codex.sock \
  --codex-auth-file "$HOME/.codex/auth.json"
```

`--codex-auth-file` names only an existing file. When omitted, the Host reuses
the runner's `openaicodex.EnvironmentConfig` file discovery: first
`OPENAI_CODEX_AUTH_FILE`, then `CODEX_HOME/auth.json`, then
`$HOME/.codex/auth.json` (with the existing user-home fallback). The CLI option
overrides environment discovery. Relative paths are made absolute in the Host
before children change directories. This interactive source accepts files;
inline `OPENAI_CODEX_ACCESS_TOKEN` / `OPENAI_CODEX_ACCOUNT_ID` and API-key
variables are not consulted. No API key, credential directory, or Ollama
process is required for this configuration.

Create the session in Terminal 2:

```sh
bin/unreal-agent attach --socket /absolute/private-runtime/codex.sock \
  --mode create --session codex-work-1
```

Enter `Reply with: Codex TUI smoke test OK` and press Enter. Confirm that the
model response appears in the TUI, then use `/detach` to leave the session
running. Reattach with the same socket and `--session codex-work-1`. After
stopping/restarting the Host with the same runtime configuration and session
directory, use `--mode resume --session codex-work-1` instead. `/resume` also
resumes a stopped session from within the TUI.

Unreal Agent only reads this externally owned file. It never logs in, refreshes,
imports, overwrites, or logs out Codex credentials. `/login openai-codex` remains
unsupported. `provider.ExternalCodex` validates and rereads the file on every
model request, including after Codex CLI replaces it. Missing, invalid, expired,
or nonprivate files yield the existing typed `external_reauth_required` error;
renew the source externally through Codex CLI. Remote authentication failures
retain the provider's sanitized error codes. Requests do not forward credentials
through redirects.

For async subagents, select the same `openai-codex` OAuth reference in the
operator's child template. Discovery also applies when only a child template
uses Codex. The Host passes the resolved path as
`child --stdio --codex-auth-file /absolute/path/auth.json`; the child opens and
rereads that private source itself. Its restricted `PATH`/`LANG` environment
does not inherit parent tokens. Credentials and the source path are kept outside
runtime resume identity, delegated task JSON, Session history and Operations.
The existing bounded child policy still applies.

## Terminal controls

- Enter submits the composer; left/right, home/end and backspace edit grapheme clusters. Bracketed paste only inserts text: pasted newlines and paste completion never execute commands or submit input. On explicit Enter, the first token (after optional leading whitespace) is matched exactly against the same command registry used by completion. Registered slash commands go to the command dispatcher, including multiline/pasted commands; a command error never resends the text as chat. Unknown slash commands, slash mentions in prose and fenced text remain normal user messages. Arguments retain their newlines, including child task text after `--`. Private credential entry remains separate and masked.
- Ctrl-D or `/detach` disconnects. `/stop` and Ctrl-C submit a normal durable hard-stop control; `/stop idle` requests stop when idle. `/resume` opens a new ownership generation after stopping.
- `/retry` resends the most recent uncertain input with its original input ID. It cannot create duplicate canonical input after an ACK was lost. A pending request can be canceled with Ctrl-C; cancellation of waiting alone does not retract an already committed input.
- Type `/` to search commands. While the command menu is open, Up/Down select a candidate and scroll the menu to keep it visible, including on short screens. Typing filters by prefix and resets the selection. Enter runs a complete candidate; for a command needing arguments, it inserts the command prefix and shows its existing usage so you can supply the arguments. Tab completes the selected candidate after arrow navigation; without navigation it retains common-prefix completion and repeated-Tab cycling, including `/child` IDs. `/help`, `/methods` and `/credentials` open a dock sheet; the next Enter closes it without submitting input. Menu navigation and completion do not apply to pasted text or private input. Up/Down leave normal input unchanged.
- PgUp/PgDn scroll the bounded conversation window by a page. Enter submits and returns to live following. The display retains the most recent 1,024 transcript entries, clips each displayed entry at a grapheme boundary to 4 KiB, and leaves full canonical history available through paged Inspect. Multiline pasted input wraps in a composer that follows the editing caret; the Rule shows hidden lines. The hardware cursor marks the editing position.
- Resize is detected without changing session state. Output is bounded to terminal rows/cells, and provider/tool text cannot inject terminal escapes. Native terminal writes observe cancellation and a two-second deadline. Rendering is independent of the subscription receiver.

## Workspace file selection

Type `@` at the start of a composer token (or after whitespace) to open **Files**.
Continue typing to fuzzy-search workspace-relative filenames; filename matches
rank ahead of directory-only matches. Up/Down select, PgUp/PgDn page the picker,
and Enter or Tab inserts the selected path followed by a space. Escape closes
the picker without changing the composer. A later Enter submits the completed
input through the usual message/command classifier. The first Enter used to
choose a path never submits input or runs a slash command.

Space/quote/backslash-containing paths are quoted; CJK, combining characters
and emoji remain intact. Use `@"docs/space fi` to search names containing spaces.
Mentions work in multiline and pasted input; paste itself never confirms a
candidate. Private input and other modal pickers take precedence. Chat, Split
and Orchestration use the same composer/picker. At fewer than 20 columns or
8 rows, confirmation waits for a larger terminal; Escape/Ctrl-D still work.

The root comes from the Session's immutable canonical workspace, including on
attach/resume; it never falls back to the attaching terminal's current directory.
Only regular files are indexed. All symlinks (including links inside the
workspace), VCS/private credential directories, common credential/key files,
`.env`/`.env.*` and unsafe terminal/bidi filenames are excluded. Nested
`.gitignore`, `.ignore` and the root `.git/info/exclude` apply with ordered
negation, anchoring, directory patterns and `**`; ignored directories are not
traversed. Invalid, oversized, unreadable or symlinked ignore files cause that
subtree to be omitted. Global Git excludes outside the workspace are not read.

Scanning is asynchronous and bounded to 10,000 files, 20,000 directory entries,
32 directory levels, 4 KiB paths and a one-second cooperative deadline. Ignore
metadata is limited to 64 KiB per file, 512 KiB total and 4,096 rules. Searches
accept up to 256 UTF-8 query bytes, return at most 32 candidates, and have a
250 ms cooperative deadline. Partial scans are identified in the picker.
Escape, scope/connection changes and detach cancel workers; generation/query
IDs discard stale responses. Reopening scans afresh. Candidate insertion
rechecks that the path is still a regular, non-symlink workspace file.

The index reads filenames and bounded ignore metadata, never source file
contents. Inserting a path creates no ToolCall, Operation, receipt, model call
or Bash execution, and grants no permission. Subsequent model/tool requests
retain the existing Registry/Permission/Operation boundary.

## Live Dock presentation

The terminal implements **C. Live Dock** from
[the TUI design specification](design/UNREAL_AGENT_TUI_DESIGN.md). An idle
80×24 screen shows the header, canonical conversation, contextual keybar,
Rule and composer. The composer ends at the bottom terminal row, including
when input spans multiple lines. Temporary streaming text has a dotted seam and a single
phase pulse; the canonical response replaces it. Tool receipts remain at their
calling step, completed calls collapse by tool, and failed/canceled calls and
spawns remain visible. No Operation ID is printed in the main display.

Child lanes appear only when children exist. At 120 columns they move to a
right rail separated by three spaces. `/child ID` focuses existing viewer
data; `/child-history` and `/child-next` page its canonical history. The normal
composer always sends to the parent, including while a child is focused.
Use `/child-send TEXT` to steer that child and `/child PARENT_ID` to clear
focus. Child state comes from the parent Operation or a canonical Finish;
unobserved activity/usage is omitted, and a connection loss shows unknown
state until observation recovers. These displays do not resume or acquire
ownership of a child.

### Chat, Orchestration and Split

`/view` opens the shared View picker. ↑↓ selects, Tab selects the next item,
Enter applies, and Esc cancels. Direct commands are `/view chat`,
`/view orchestration`, and `/view split`. Left/right arrows continue to edit
the composer. View preferences and scroll positions are terminal-local; they
do not change Session configuration, history, permissions or runtime selection.
A new attachment starts in Chat.

- **Chat** keeps Live Dock, conversation scrollback and its existing child
  dock/rail/focus behavior.
- **Orchestration** uses the body for the current parent, children and
  Unreal-owned Operations. PgUp/PgDn scroll this bounded topology from top to
  bottom; switching back to Chat retains its separate transcript position.
- **Split** keeps Chat on the left and Orchestration on the right. It requires
  **120 columns** (Wide/XWide). The right pane uses 34–44 columns at 120–159 and
  48–60 columns at 160+, while Chat retains its maximum body width. PgUp/PgDn
  scroll Chat; the right pane follows an active path with parent/ownership
  context. Use Orchestration-only to browse all projected nodes. Chat's child
  rail/dock is suppressed in Split to avoid duplication.

The header names the effective view. If a selected Split view becomes narrower
than 120 columns, Chat is shown with a short hint; widening restores Split.
The narrow View picker omits Split, and a direct narrow `/view split` reports
the required width. The keybar, separator, composer and hardware cursor remain
at the bottom in every view, including multiline and masked credential entry.
Tiny/Narrow terminals, NO_COLOR, plain, ASCII, CJK/graphemes and the reserved
rightmost column use the same Live Dock rendering rules.

The tree shows each agent's own provider, model selection, effort and runtime
revision. A known observed model is separate from the selection. Public task
labels are shortened without generating summaries. Status words and symbols
remain understandable without color; active paths are emphasized and completed
nodes are faint. **Unreal Host validates and owns requests**: parent-to-child
edges represent Host-owned delegation, not models calling one another.
Operation targets use the same whitelisted display arguments as tool receipts.
Unknown/resync/disconnected observations are shown as such; a canonical Finish
remains visible without inventing live activity.

Orchestration is a read-only projection of current Host snapshots, indexed
viewer rows, canonical runtime records and safe tool-receipt linkage. It never
reads Context Engine selections as history, writes canonical records, creates
Operations, changes provider selection, or invokes a model. It excludes message
bodies, hidden reasoning, continuation state, raw tool state/arguments/results,
environment, credentials, account identity and internal error bodies.
Credential-shaped labels/targets are protected rather than displayed.

While Orchestration or Split is visible, the viewer additionally inspects at
most eight direct children per one-second polling batch, round-robin, alongside
the existing selected-child observation. Canonical completion stops background
polling of that child. This is inspection only; detach stops frontend observers
and leaves Host/children untouched. Chat keeps its previous observation scope.
Nested lineage is displayed when present in existing child snapshots, with
cycle/missing-parent protection. The existing gateway authorizes direct-child
inspection; unobserved nested activities remain unknown rather than widening
that boundary.

This is a bounded live topology, not an all-session DAG. Projection retains
active paths and recent Operations before older terminal nodes, with a
screen-size-dependent limit (64–512 nodes) and an overflow count. Redraw reads
indexed snapshots and bounded display receipts, not full canonical history.
With many children, observation may lag a polling round. Safe target linkage
may be unknown if its display cache is unavailable; raw state is never used as
a fallback. `/analyze` remains analytical detail; `/export` remains public
conversation Markdown export. Orchestration has no export of its own. Opt-in
[Claude Tool Bridge](claude-tool-bridge.md) Operations use this same projection.

日本語: `/view` で Chat / Orchestration / Split を選択できます。
Orchestration は親・子それぞれの provider/model/effort、短い依頼名、状態、
Unreal が所有する Operation を表示する通常利用向け画面です。
モデル同士が直接呼び合う構造ではありません。120列未満では切替のみ、
120列以上では左右 Split が使えます。composer は画面下段のままです。
PgUp/PgDn は Chat/Split では会話、Orchestration では実行構造をスクロールします。
表示は read-only で、Session/Operation/Permission の所有権を変えず、
本文・認証情報・private reasoning・raw tool data は投影しません。
detach/resume 後は canonical な状態から再表示し、View 選択は保存しません。

For a manual viewing smoke, attach an existing running Session with the updated
binary, then open `/view`, select Chat/Orchestration, widen to 120+ columns and
select Split. Check PgUp/PgDn, resize back below 120, and Ctrl-D/reconnect.
Mixed providers, nested/concurrent children and Operation transitions are
covered by fake-data/integration tests. For real child execution, use the
explicit operator commands and dedicated child smoke configuration documented
in [Multi-provider runtime and child smoke](multi-provider.md#実機-smokeユーザーが実行).
Those commands are user-run model requests; viewing alone sends none.

### Transcript scrollback and Markdown conversation export

In normal conversation, PgUp/PgDn move one transcript viewport at a time,
including wrapped Markdown, code, CJK and grapheme lines. Reaching the bottom
resumes live follow. New streaming data preserves the first visible line while
you are reading older content; resizing preserves the bottom-relative offset.
Sending a new prompt returns to live follow. Analysis/help sheets and pickers
use their own paging; Esc closes help or goes back from analysis.

The live terminal projection retains at most 1024 entries / 2 MiB. PgUp at its
oldest boundary loads a bounded preceding page from canonical Host history;
PgDn walks back toward the live tail. Old-page reads do not move the subscription
cursor, alter Session state, or use the Context Engine. Each displayed message
is capped at 64 KiB with an explicit clip notice. The right terminal column is
still reserved, and the composer stays at the bottom.

Use these conversation export commands:

- `/export last` saves the latest public agent response as Markdown. If its
  body is protected or otherwise ineligible, it reports a safe failure; it
  never silently substitutes an earlier response.
- `/export` opens **Export response**, a newest-first picker of exportable past
  agent responses. Each choice has its canonical turn number (when known) and
  a short preview. ↑↓ selects and scrolls, Tab selects the next item, Enter
  exports, and Esc cancels. There is no need to enter a turn number.
- `/export all` saves the Session's full public user/agent conversation in one
  Markdown file, with Turn / You / Agent sections and known timestamp,
  provider, model selection, effort, and observed model metadata.

All three read canonical Host history through paged inspection. They capture
an append-only history prefix at command invocation; subsequent messages can
be exported with a new command. Rendering, screen clipping, wrapping, colors,
scroll position, the 1024-entry display window, and Context Compaction omissions
have no effect on the source. Streaming drafts are excluded. A response with
multiple public message parts joins them with two newlines. Conversation export
always targets the parent Session, including while a child viewer is focused.

Files go to `$XDG_STATE_HOME/unreal-agent/exports/` or, when unset,
`~/.local/state/unreal-agent/exports/`. The owned directory must be **0700** and
files are **0600**. Filenames follow the shared private export convention:
`response-<random>.md` for a response and `session-<random>.md` for a conversation.
No session/user text appears in filenames. Existing files and links are never
replaced. Writes use a private temporary file, fsync, and atomic no-replace
publication; failed or canceled writes leave no completed partial file.

Full-conversation export streams message bodies to disk and retains only small
turn metadata/previews, rather than assembling one session-sized string. There
is no aggregate 8 MiB export cap or silent truncation. History/gateway, disk,
permission, and cancellation failures produce a safe `export failed: ...`
notification. The gateway's existing per-page transport limits still apply.
Success produces the TUI-local `exported to <path>` notification; no export
command, notification, or Operation is appended to canonical history.

Only public external user messages and public assistant responses are eligible.
Credential entry, controls, peers, account/auth metadata, raw tool arguments and
results, provider replay state, hidden reasoning and internal compaction messages
are excluded. Credential-shaped bodies are excluded in full rather than partly
redacted. Terminal/bidi control characters are removed by the existing SafeText
policy; otherwise UTF-8, line breaks, Markdown/code fences, CJK, emoji and literal
HTML-looking text are preserved. The response picker contains only eligible
responses, including responses older than the live terminal window.

`/analyze` → Export remains a separate JSON/Markdown **metadata/statistics**
report and includes no conversation bodies. It shares only the private file
writer with `/export`.

履歴は削除せず、PgUp/PgDn で古い canonical 会話を読みます。
`/export last` は最新の公開 agent 回答、`/export` は picker で選んだ過去の
回答、`/export all` は Session の公開 user/agent 会話全体を Markdown に
保存します。表示範囲や Context Compaction の選択範囲には依存しません。
private entry・credential・reasoning・tool の内部データは対象外です。
保存先は private な state/exports directory で、通知も TUI 内だけです。
`/analyze` の Export は引き続き本文を含まない統計 report です。

Below 70 columns the layout contracts; below 50, roles occupy their own lines
and child/tool activity becomes a compact summary. Short screens retain
conversation space and show the Ctrl-C hint beside the composer. The terminal
reserves the rightmost column and updates changed rows in place, including
during streaming, rather than clearing the whole screen.

The default theme uses ANSI foreground colors and bold/faint/reverse attributes
with no background colors. Set presentation options on the **attach** process:

```sh
NO_COLOR=1 bin/unreal-agent attach --socket /absolute/private-runtime/codex.sock --session codex-work-1
UNREAL_AGENT_TUI_PLAIN=1 bin/unreal-agent attach --socket /absolute/private-runtime/codex.sock --session codex-work-1
UNREAL_AGENT_TUI_ASCII=1 bin/unreal-agent attach --socket /absolute/private-runtime/codex.sock --session codex-work-1
UNREAL_AGENT_TUI_NO_ANIMATION=1 bin/unreal-agent attach --socket /absolute/private-runtime/codex.sock --session codex-work-1
```

Nonempty `NO_COLOR` removes color SGR while retaining attributes; `TERM=dumb`
or plain mode removes all SGR. A non-UTF-8 locale selects ASCII chrome
automatically. Explicit ASCII is also useful for terminals that render
ambiguous-width rules/ellipsis as two cells. Conversation text remains Unicode.
Disabling animation substitutes static marks while keeping phase/state words
and known elapsed times.

HTTP(S) Markdown links and bare URLs in public conversation text are clickable
when the terminal advertises supported OSC 8 hyperlinks. URLs remain visible as
plain text in unknown terminals, tmux/screen and plain mode. `NO_COLOR` removes
colors but retains supported links; ASCII mode changes chrome, not conversation
text. Links retain grapheme-safe wrapping in Tiny and Split views.

Only the renderer generates OSC 8, after URL validation. Userinfo, malformed
authorities, control/bidi characters and non-HTTP(S) schemes are rejected. Raw
provider ANSI/OSC sequences are stripped; sanitized messages containing such
input do not become clickable. URLs are limited to 4096 encoded bytes and bare
link detection to 256 candidates per line. Non-ASCII hostnames must use their
punycode form; Unicode paths and labels are supported. The composer and code
examples stay text. Unreal never launches a browser: opening a link is an
explicit terminal action. Canonical history and Markdown exports keep their
original public message content.

## Model and reasoning selection

`/model` opens a model picker. Up/Down select and keep the selection visible;
Enter advances to that model's reasoning-effort picker. Enter there confirms
both values. Tab advances the picker selection. Esc returns from effort to
model, or closes the model picker; cancellation leaves the runtime unchanged.
The existing slash menu still uses Tab to complete its selected command.

For `openai-codex`, the source is Codex CLI's `models_cache.json` beside the
resolved external auth file: normally `~/.codex/models_cache.json`, or the
corresponding `CODEX_HOME` / explicit auth-file directory. Unreal Agent reads
the CLI cache on each picker open and validates the selected entry again on
confirmation. It never fetches/refreshes the catalog, runs a login, or reads
cache account identity/instructions into the UI. Only entries with CLI
visibility `list` and their declared reasoning levels are offered. Levels
unsupported by the existing harness (currently including `ultra`) are omitted
with an explanation in the effort picker. The list is not a subscription/quota
check or a promise that an old CLI cache still matches the backend.

When the cache is missing, invalid or unavailable for the current provider,
the picker says `catalog unavailable` and offers only the current model and
its current effort. It does not substitute a guessed catalog. A nonstandard
auth-file directory without a CLI model cache has the same behavior.

For `claude-code`, `/model` discovers selectable models/efforts through the
installed CLI's isolated `initialize`/`list_models` control metadata, without
inference. Claude applies organization model restrictions. A one-minute Host
cache avoids a subprocess on every open; `/model refresh` forces an update.
The picker shows `discovered catalog`. Optional `Runtime.ClaudeCode.Models`
entries remain the explicit fallback when discovery is unavailable; without
them only the current selection is shown. A successful organization catalog is
not expanded with configured/current models. Alias values are canonical choices;
resolved IDs are catalog metadata, and completed responses retain their actual
observed model. See [Claude model discovery](claude-code.md#model-picker-and-analysis).

Other providers retain their existing authentication and inference support; catalog
discovery/model switching beyond their current selection is not implemented.

With multiple registered backends, `/model` first opens a Provider picker, then
uses that provider's model/effort catalog. Namespaces and authentication stay
separate. See [multi-provider setup](multi-provider.md) for config and children.

Confirmation appends a nonsecret `runtime_selection` intent with a monotonic
revision and an idempotency ID. It does not trigger inference. The coordinator
appends `runtime_applied` immediately before the first request at a completed
turn boundary, and each canonical Turn references the active revision. A busy
model request and its tool loop finish with their current selection; a pending
choice is shown as `next turn`. Multiple pending confirmations replace the
pending choice through ordered revisions, while an obsolete picker is rejected
and must be reopened. The current model/effort appears in the header when space
allows and yields to session/connection state on narrow screens.

Resume/restart replays the latest applied choice and any still-pending intent.
An interrupted loop resumes with its prior choice before applying the pending
one at the next boundary. Keep the **original** runtime JSON when restarting
the Host: the creation identity comparison remains strict. Prior turns and
their selections are retained; credentials, endpoint, permission policy,
profile and subagent templates remain unchanged. Existing durable settings
controls still replay in canonical order. Sessions using the new selection
records require this version of the binary; older records without a revision
continue to mean the creation selection. Each Turn in this store is one model
request, so a tool loop can contain multiple analysis rows.

## Session analysis and metadata export

`/analyze` opens one read-only picker: Overview, Usage, Turns, Tools, Agents,
Context, Timeline, Errors and Export. Up/Down scroll the selection, Enter opens
a view, and Esc/Enter returns to the picker. Inside a view, Up/Down and PgUp/PgDn
scroll wrapped rows. Esc at the picker returns to the conversation. Host/child
observation continues while a sheet is open. The composer stays at the bottom.
There are no separate `/effort`, `/usage`, `/stats`, `/tools` or other statistics
commands, and analysis text is never added to conversation history.

Usage displays the existing viewer totals for input, cached input, cache-write
input, output, reasoning, responses, Known and Partial. Unknown tokens are
shown as `unknown`; partial totals use `~`. Turns map each response by its
canonical TurnID and show the model/effort active at that Turn's revision,
including existing settings controls. Tools count observed calls and their
canonical terminal outcomes, with elapsed time only when both timestamps are
known. Agents show existing observed child usage, Operations, elapsed, Finish
and parent Operation status; undiscovered/unobserved activity is never inferred.

Context now includes the bounded Context Engine's last prepared request: original
unit/selection/retrieval/reference/omission counts, checkpoint boundary, estimated
input/budget/utilization and retrieval latency. These are disposable metadata,
not canonical history or actual measured usage. After Host restart, package
diagnostics are unknown until a new request is prepared. See
[Context Compaction v1](context-compaction.md) for configuration, security,
rebuild and long-session smoke steps. Complete canonical history is retained.

Context charts also show observed request input usage with Unicode or ASCII bars.
Context-window ratios appear only with reliable CLI catalog metadata recorded
for that selection and complete usage for the latest observed Turn. Timeline
uses canonical record timestamps and observed child Finish timestamps. Errors
count canonical model failures, failed/canceled tools, child failures and
`InputCrash` records. Transient connection problems and nonpersisted Host
failures are not counted as canonical errors; error bodies are omitted. No
remaining subscription quota, billing balance or token-to-currency estimates
are displayed.

Export offers JSON and Markdown for all eight analysis views in one report.
Reports contain metadata/statistics only: no prompts, responses, child-message
bodies, tool arguments, Operation state, error bodies, auth fields or cache
account identity. JSON is versioned and durations use nanoseconds; Markdown
uses readable durations. Files are created exclusively with 0600 under
`${XDG_STATE_HOME:-$HOME/.local/state}/unreal-agent/exports` (0700, owned by the
current user). The resulting path is shown in the TUI. Existing files are not
overwritten, and nothing is written to the workspace. This export location is
independent of an explicit launcher `--state-directory`.

The projection retains at most 4,096 Turn rows, 65,536 tool calls, 512 timeline
entries and 128 recent errors. Hitting a bound or observing an incomplete/resync
page marks the report partial. Canonical history is unchanged and no aggregate
statistics are persisted in Session state.

## Private authentication

The installed Claude subscription backend uses provider `claude-code` and the
external reference `claude-code` / `oauth` / `external-claude-code`. Authentication
belongs to the CLI; `/login` does not import Claude credentials. This backend is
text-only by default, discovers the CLI's model/effort catalog with optional explicit fallback, and reuses `/model` canonical
selections and `/analyze` usage/statistics. See [Claude Code backend](claude-code.md)
for the invocation, permissions, isolated smoke config and managed-policy
diagnostics investigation. `Runtime.ClaudeCode.managedPolicyMode` defaults to
`reject`, preserving fail-closed preflight for existing policy sources, unknown
evidence and Team/Enterprise remote policy delivery. A doctor's no-policy
snapshot cannot bind the next `-p` process. Explicit `trust` instead permits
Team/Enterprise by accepting organization-managed policy as an external trusted
administrative boundary; it does not certify that policy safe. Claude built-in
tools remain disabled. `Runtime.ClaudeCode.ToolBridge.Enabled=true` explicitly
enables only [Unreal-owned tools](claude-tool-bridge.md) through SDK MCP; it grants
no permissions and does not alter existing text-only configs. `/analyze` Overview
reports this safe capability metadata. Organization-managed
hooks, env, telemetry, helpers and policy refresh may act outside Unreal
Operations. Parent credentials, billing/routing overrides and OTel settings
are still isolated; trusted managed OpenTelemetry is applied by Claude itself.
The read-only `/analyze` Overview and exports show the nonsecret policy mode.

`managedPolicyMode=trust` は、ユーザーが組織管理者 policy を外部の信頼境界として
明示的に信頼する設定です。Unreal Agent が安全性を検証した意味ではありません。
Claude built-in tools は無効のままですが、組織 managed hooks、env、telemetry 等の
副作用は Unreal Agent の Operation 境界外で起こり得ます。

The OpenAI API-key path remains optional and separate from the external Codex source. To use managed credentials, pass `--credential-directory /absolute/private-credentials` to `serve`. For OpenAI select `provider: openai`, a supported explicit model/family, the OpenAI endpoint/network origin, and `auth: {"provider":"openai","method":"api_key","id":"primary"}` in the runtime configuration. No key belongs in this file. A Host can configure both sources; only the exact `openai-codex` / `oauth` / `external-codex` reference resolves through the external auth file, while managed references continue to use the private store.

In the TUI use `/login openai primary`, then enter the key at the masked prompt. Ctrl-C cancels private entry. `/logout openai primary`, `/credentials` and `/methods` use the same UI-neutral authentication service as the authentication CLI. The support matrix explicitly reports supported/configuration-only, unsupported and deferred flows. OAuth/browser login is not implemented. Logging out removes managed credentials; external source ownership is unchanged.

Keys pass over the private local socket to `authflow.Service`, never as Inbox input, tool arguments, Session history, screen text or command history. Secret buffers are cleared where mutable; Go strings and transport buffers are transient memory, not a promise of forensic memory erasure. Authentication methods are validated before private entry. A stored key is checked by the next real provider request, not by a speculative login-time inference call.

## Projection and progress contract

The wire protocol is versioned `/v1/...` HTTP over the private socket. Submit returns the existing durable Host receipt and checks generation. Requests are capped at 128 KiB; pages at 256 items; frames at 32 MiB; subscriptions at 128 queued events. An oversized canonical item/frame produces an explicit error rather than truncated/invalid JSON. The server uses a write deadline and the Host's existing bounded subscription/gap mechanism. A slow client owns no execution lock.

The receiver subscribes atomically, pages canonical history by sequence, and replaces operation snapshots with the latest owner view. Duplicate sequences/revisions are ignored. Gaps, changed generation and connection loss trigger a visible resync; disconnected runtime status is unknown. No frontend state is written to disk. Reconnect retries every 250 ms and stops on client cancellation.

`llm.RequestOptions.Progress` is optional. The Responses adapter emits only text deltas and attempt-reset markers. Host progress is ephemeral, keyed by generation, turn, model-call epoch and transport attempt, capped at 64 KiB and emitted at most about 33 times/second plus reset/completion markers. Late/canceled calls cannot overwrite newer progress. Completed model responses alone enter canonical history; canonical completion replaces temporary text. Adapters without this callback show an explicit completed-response waiting state.

`agentrunner.NewRuntimeFactory` creates fresh per-session components; `RuntimeConfig.NewTools` lets composition inject protocol/subagent handlers with the same owner context. Shared protocol managers belong to the Host composition and are closed after `Host.Close`; each session closes only its own handlers.

`tui.Config.Viewer`, `Command`, and `Updates` plus the bounded gateway extension
endpoint connect existing structured viewer rows and history pages. The old
`Panel` string hook remains available to embedders. Layout, sheets, scrollback,
completion, analysis and text wrapping caches belong to the frontend. The only
new canonical state is the explicit model/effort selection history and its
Turn revision reference; analysis adds no canonical aggregates and changes no
permission, Operation, credential or child ownership semantics.

## Validation

`make test check build` runs race tests, vet, formatting and builds. Added tests cover real subprocess attach/detach, durable retry, multiple prompts, writer contention, resume generations, slow subscribers, bounded transient progress, real SSE retry/reset, private authentication, gap rebuild, Unicode/paste/resize, and cancellation. Linux additionally runs a real PTY subprocess with Unicode input, resize, masked login and detach. The Darwin build is checked with `GOOS=darwin CGO_ENABLED=0 go build ./cmd/unreal-agent` on this pre-AST branch; final AST composition has its separate CGO requirement.

## Structural, semantic and debugger tools

The common runtime includes the native file and Tree-sitter AST tools used by
the one-shot runner. Full AST support requires the normal CGO build.

The serve JSON may additionally contain `LanguageServers` (the entries described
in `harness/lsp/README.md`) and `DebugAdapters` (entries described in
`harness/dap/README.md`). Language servers are shared by sessions in this Host
and workspace; debugger owners are isolated per Session runtime. All protocol
work remains in Operations. Session cleanup drains request handlers and debugger
resources; the Host closes shared language servers after its sessions stop.
Configuration digests are included in immutable runtime identity. Protocol
settings come from this explicit serve configuration, not client prompts.

Example optional top-level fields:

```json
{
  "LanguageServers": [{"language":"go","path":"/opt/bin/gopls","extensions":[".go"]}],
  "DebugAdapters": [{"id":"lldb","path":"/opt/bin/lldb-dap","directory":"/workspace"}]
}
```

Combine these with the Runtime and Permissions fields above. Enabling a tool
does not grant process permissions; the explicit execution policy is checked
at every boundary. Restrictive OS sandbox requests remain unsupported.

### Multi-provider runtime and children

An optional top-level `Providers` map registers additional backends alongside
`Runtime.Provider`. `/model` then uses Provider → Model → Effort; canonical
runtime revisions also bind provider changes. `/analyze` and child projections
identify each provider separately. See [multi-provider configuration, permission
boundaries, compatibility and smoke commands](multi-provider.md).

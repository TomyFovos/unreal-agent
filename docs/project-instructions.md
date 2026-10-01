# Project instructions (Issue #27)

The harness automatically applies `<workspace root>/AGENTS.md` as project
instructions: guidance that holds for all work in the project. It is separate
from skills, which the model loads on demand with SkillUse, and from the task
itself (the user prompt or delegated packet). No other file is discovered:
CLAUDE.md, GEMINI.md, copilot instructions, nested AGENTS.md, RULES.md,
@import, and provider-specific shadowing are out of scope. The harness consumes
the rendered AGENTS.md only; how a project generates it is not its concern.

## Session binding

Discovery happens once, when the Host creates a session
(`host.Options.Workspace`). The resulting `projectinstructions.Snapshot` is
published atomically with the new session header as a version 1 host record of
kind `project_instructions`:

| Field | Meaning |
|---|---|
| Version | snapshot schema, currently 1 |
| SourceKind | `none` (no AGENTS.md) or `workspace_agents_md` |
| SourcePath | `AGENTS.md`, relative to the workspace root |
| Content | the exact file text |
| Digest | `sha256:` of Content |
| ByteLength | length of Content in bytes |

The Coordinator replays this record into the ContextBuilder like any other
history item. Resume, Host restart, and fork therefore never reread the
filesystem: a session created with revision A keeps A after AGENTS.md changes,
and only a fresh session sees revision B. Fork copies the parent history,
including the record. A child session is created with
`host.Options.ProjectInstructions` set to its parent's snapshot, so a parent
and child cannot silently split onto different revisions. The snapshot is not
embedded in the delegated task. Sessions created before this record existed
have no project instructions and are not rediscovered on resume.

Validation on load rejects a record whose digest or length does not match its
content. A session log with this record needs a harness that understands it;
older binaries fail explicitly on the unknown host record kind.

## Safe read

`projectinstructions.Discover` opens the workspace directory (the configured
path may itself be a symlink) and reads `AGENTS.md` relative to that
descriptor with O_NOFOLLOW and O_NONBLOCK. A missing file binds `none` and
the session starts normally. Every other problem fails creation with a typed
`*projectinstructions.Error`, and no session is created:

| Code | Cause |
|---|---|
| symlink | AGENTS.md is a symbolic link, inside or outside the workspace |
| not_regular | directory, FIFO, device, socket, or hard-linked file |
| oversized | larger than 64 KiB; content is never truncated |
| binary | contains NUL bytes |
| invalid_encoding | not valid UTF-8 |
| unreadable | open, stat, or read failed |
| invalid_workspace | workspace is relative, missing, or not a directory |

Over the gateway these surface as `project_instructions_<code>`.

## Context layering

The system message is rendered from separate builder layers, in stable order:
harness preamble, caller system prompt (after model profile composition),
project instructions (`Builder.SetProjectInstructions`), then the skill
catalog. The layers render into a single system message, so the prompt-cache
prefix changes only when a layer does. Lifecycle guidance edits only the
harness layer. Empty or whitespace-only AGENTS.md renders nothing.

## Authority

AGENTS.md is model behavior guidance, not a security boundary. It is never
consulted for permissions, tool enablement, network capability, credentials,
lifecycle, or session authority; the #14 permission policy enforces every tool
request regardless of what the instructions say. Model profiles (#13) do not
decide whether AGENTS.md is loaded. TUI and gateway clients never read the
file; `host.View.ProjectInstructions` exposes only non-content metadata
(source, digest, size, snapshot version).

Tests cover missing, present, and ignored surfaces; resume, restart, fork,
fresh-session, and child inheritance across an on-disk change; symlink
(escape, inside, dangling), directory, FIFO, hard link, oversized, binary,
invalid UTF-8, unreadable, and invalid workspace; tampered snapshots;
capability isolation under a deny-all policy; skills alongside AGENTS.md; and
the one-shot runner's output stream.

## Public history projection

Host Inspect, Subscribe initial views/events, gateway frames, and Host-backed
CLI logs use host.HistoryItem / host.HistoryPage. A project instruction history
item keeps its canonical kind, sequence and timestamp, but contains a
host.ProjectInstructionRecord with source kind/path, digest, byte length and
snapshot version only. Paging cursors, revisions and gap handling are unchanged.
Canonical sessionstore.HostRecord still persists the complete Snapshot and is
the only replay source for Resume, Restart, Fork and Coordinator binding.
A public metadata record cannot be decoded as a complete canonical Snapshot.
Inherited snapshots embedded in child configuration records are also projected
to metadata; canonical configuration bytes remain unchanged.

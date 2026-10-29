# Human-approved Session permissions

`/permissions` uses the native picker, with the current mode visible in the
header (RO/ASK/FULL in Tiny). Both Codex and Claude use Unreal-owned Operations,
executors and receipts. Claude StructuredOutput remains a serializer; its
side-effecting built-ins and foreign MCP stay disabled.

| Mode | Execution |
| --- | --- |
| Read Only | Initial mode. Configured read/grep/glob inspect the workspace, and shell commands that only read run (below); other privileged Operations receive typed permission denial. |
| Ask for Approval | Each privileged Operation waits for Allow once (initially selected) or Deny. The dialog shows the command or file first, then the Session and request. |
| Full Access | Selecting the mode prepares a separate warning dialog. Explicit confirmation grants this Session access up to the reviewed operator ceiling. |

Shell commands that only read run in every mode without approval, as Codex
runs its known-safe commands, for Unreal's Bash, children and Claude Code
alike. Each command in the line must be one of a fixed set of read-only
programs (git status/log/diff/show/branch listing and a few other git reads,
ls, cat, grep, rg, find, sed -n, head, tail, wc and similar), without an
option that writes, deletes or runs another program, joined only by |, &&, ||
or ;. Expansions, substitutions, subshells, assignments and redirections other
than to /dev/null or 2>&1 make it an ordinary command
(`harness/permission/readonly_command.go`).

Paste and chat cannot approve. Approval requires at least 80x24 so warnings can
be reviewed. Tiny can deny and return to Read Only. Escape dismisses a pending
dialog without approval; `/permissions` can reopen it. Unanswered requests deny
after two minutes, configurable up to one hour. Cancel and detach refuse pending
requests. Approval is bound to one Operation; later work needs its own approval.

Full Access and approved Bash are **not an OS sandbox**. Bash can execute arbitrary
commands and access the ambient filesystem, network, credentials, environment
and shared mounts. Git push is technically possible: separate Git operating
approval rules still apply, without permission-based Git isolation claims.
Native file/AST tools retain workspace semantics. Immutable parent restrictions,
runtime tool exclusions and the operator maximum still apply.

Full Access continues after detach in that Host lifetime. Returning to Read Only
revokes active privileged leases and requests cancellation of processes,
native/AST work and child starts. Completed/external effects cannot be undone.
Canonical Operations record the actual grant and terminal outcome. The mode last
selected in a workspace is kept: Host restarts, updates, resumed Sessions and
new Sessions start in it (U20). Private approvals are never persisted/replayed. Completed receipts remain durable. Incomplete restored
Operations are canceled with an expired permission decision, not re-executed.
Inspect history and submit a new request to retry interrupted work.

Stopped Session snapshots expose effective Read Only with an expired status;
the previously selected mode is historical metadata, not active authority.
The native picker shows Session stopped / permissions expired. A resumed
generation requires fresh approval, while past Operations keep their decisions.

## Configuration and compatibility

New launcher bootstrap creates a read-only baseline plus a capability ceiling;
it never stores a selected privileged mode or approval. Existing files are not
overwritten. Legacy explicit policies, including unrestricted ones, keep their
behavior unless the operator opts in. `/permissions` shows a configured-policy
hint for legacy Sessions. Keep runtime/auth/bridge settings and add a reviewed
ceiling to opt in, for example:

```json
{
  "Permissions": {
    "Tools": ["read", "grep", "glob"],
    "ReadRoots": ["/absolute/project"],
    "NetworkOrigins": ["https://chatgpt.com"]
  },
  "PermissionModes": {
    "Enabled": true,
    "ApprovalTimeoutMillis": 120000,
    "Maximum": {
      "Tools": ["read", "grep", "glob", "write", "edit", "Bash"],
      "FilesystemUnrestricted": true,
      "NetworkUnrestricted": true,
      "ProcessMode": "unrestricted"
    }
  }
}
```

The ceiling is **not a permission grant from a file**. Modes cannot be set in
configuration; enabled configurations require an explicit maximum. Restart the
specific Host to load configuration changes. Tool schemas reflect the ceiling,
so providers can request work, see denials and continue. Context constraints
refresh at turn and structured-generation boundaries, without switching the
turn-pinned runtime or granting authority.
No authentication is copied/imported and no provider fallback is added.

## Control boundary and children

The private Unix gateway checks the peer OS UID (Linux/macOS). A private attachment
lease, one-use Full Access challenge, Session generation and revision prevent
replays, cross-Session approvals and stale responses. Latest permission attachment
owns control. Private authority never enters public projections, model context,
history or export; only mode and canonical decision metadata are retained.

This is a trusted same-user control boundary, not proof of physical human presence
against a compromised OS account. Already-approved ambient Bash can access
same-user resources; no protection against this is claimed without an OS sandbox.
Provider output and ordinary chat have no permission approval dispatch path.

Children are workers (U11) and follow their parent's mode, also when it changes
while they run. A template's Policy is the child's ceiling and its Baseline what
Read Only and Ask allow without approval; the operator maximum bounds both. In
Ask, a child's privileged request goes to its parent agent, never to the human
(U17): the parent's model answers it (SubagentApprove, or
`unreal-agent subagent approve` from Claude's Bash) and the child waits until it
does. Starting, steering and answering children needs no approval itself, and a
mode change does not stop a running child. A child gets the Host's environment.
LSP/DAP or custom remote handlers without operation-scoped execution fail closed
under these modes; legacy configuration behavior remains compatible.

Sandbox acceptance uses fresh isolated asp/devlore config/state/socket/workspace
and Sessions, actual deny/allow receipts, file/Bash tests, detach/attach and Host
restart. A model's prose refusal alone is not a permission test. Keep all default
Hosts, old Session stores, provider CLI configuration and authentication intact.

# Capability enforcement

Hosts choose a policy once for each execution ownership lifetime. A policy is
immutable; on resume build a fresh policy from the currently accepted settings.
Persisting a tool name or an operation does not persist a grant.

```go
policy, err := permission.New(permission.Config{
    Tools:          []string{"ViewImage", "SkillUse"},
    ReadRoots:      []string{workspace, skillsDirectory},
    NetworkOrigins: []string{"https://api.openai.com"},
})
// Handle err. Close the policy after all executors have stopped.
ctx = permission.WithPolicy(ctx, policy)
manager := operation.NewLocalOperationManagerWithPolicy(ctx, policy)
```

The zero Config and explicit nil policy deny access. The Host may consciously
select `permission.Unrestricted()` instead. There is no fallback from a rejected
restricted policy to unrestricted execution. For source compatibility, original
low-level constructors and primitive calls with an *unscoped context* retain their
historical unrestricted behavior. New Hosts and resource owners must always attach
their accepted policy, including when they explicitly choose unrestricted mode.
This boundary trusts embedding Go code; it is not a sandbox for malicious Go
extensions that discard the context or call the operating system directly.

## Concrete boundaries

- The Coordinator checks the tool capability before invoking a pure Translator.
  The manager independently checks the recorded origin and built-in operation
  type before starting or resuming work. Expected denials become failed Operations
  with a serializable `Denial`, not fatal `Manager.Add` errors.
- ReadFile and Create primitives enforce filesystem policy. ViewImage and SkillUse
  therefore cannot bypass policy through direct Operation dispatch. Read/write
  roots are independent; O_RDWR requires both. Roots are existing directories
  pinned by `os.Root` descriptors. OpenFile and OpenParent use descriptor-relative
  access, so replacing a path or racing a symlink cannot redirect them outside the
  selected root. Parent traversal is rejected for restricted access. Directory
  roots grant access to entries within them, including hard links; they are not a
  claim that an inode has no aliases outside the root.
- `CheckPath` is **preflight only**. Native mutation, AST and LSP executors must
  also use pinned rooted descriptors and recheck authorization within their
  mutation lock. `OpenParent` only authorizes the target; use basename-relative,
  no-follow operations on the descriptor it returns.
- StartProcess checks before opening output captures or launching anything.
  No OS sandbox is implemented. Restricted process execution fails with typed
  `unsupported`; denied execution fails with typed `denied`. To launch an
  arbitrary shell, LSP server, DAP adapter, debuggee or Subagent, the Host must
  explicitly accept unrestricted filesystem and network effects as well as
  process execution. Environment, descendants and ambient OS resources are part
  of that explicit unrestricted grant. A program being read-only by convention
  is insufficient grounds to run it under a restricted grant.
- RemoteClient checks the policy attached to its request context. Every exchange,
  including HTTP redirects, is guarded. Provider requests preserve typed denials
  through the Responses adapter. Exact HTTP(S) origins are allowed, not wildcard
  suffixes. HTTP userinfo is rejected; denial records omit URLs, paths, commands,
  headers and credentials. Origins constrain the requested host/port, not IP
  ranges or DNS answers.
- `Policy.RoundTripper` is the same boundary for OAuth and other host HTTP.
  Default restricted transports disable ambient proxies. A caller-supplied
  transport is trusted host code and must not reroute through unauthorized
  destinations or perform hidden requests. Credential middleware remains
  responsible for removing authorization on cross-origin redirects.

## Child and resource ownership

`WithPolicy` intersects a previously attached policy and never widens it, even
if a child asks for Unrestricted. `Policy.Intersect` exposes the same rule for
composition. Separate processes must receive the accepted constraints from the
parent and reconstruct/enforce them, never select grants from their own request.
A subprocess without a supporting sandbox is rejected under any filesystem or
network restriction. No stored capability reference is a permanent grant.

Custom RemoteJob handlers must preserve this context and enforce concrete
resources at execution time. Use `operation.Authorize` before dispatch and
`operation.PermissionFailure` for terminal typed failures, then use the guarded
primitives or equivalent enforced file/process/HTTP boundaries. Registration in a
tool catalog never grants executor capability.

The normal registry's execution selection remains unchanged. Its separate
`ResolveHistory` catalog retains configured translators for history decoding and
result formatting after a tool is hidden. Third-party registries may implement
that optional method; otherwise their existing Resolve behavior is preserved.

## Tests and limits

Tests cover direct primitive calls, path escape, symlinks, root identity
replacement, nested grants, redirected HTTP, secret-free denials, subprocess
capture side effects, manager continuation, narrower resume, hidden-tool history,
and a Coordinator that accepts another input after a real executor denial.
No semantic approvals, gates or publishing authority are introduced.

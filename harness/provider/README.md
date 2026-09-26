# Provider composition

provider.New(provider.Defaults(catalog)...) reuses the existing five clients and
the completed-response llm.Adapter. The explicit catalog is supplied by the
embedding authority; the harness does not invent model availability. Public
composition rejects models outside that catalog. The legacy runner accepts the
explicit request/environment model as a one-entry catalog, so server-rejected
model names return a normalized invalid-request result without fallback.

Registry.Build resolves a versioned, non-secret Selection and returns it for
Host metadata. Persist that result and call ValidateResume before resuming.
Selection includes endpoint, credential reference, retry bound, declared model
family/capabilities and the source of selection. Configured defaults are explicit.
Credential material is resolved for each logical model request, then used by an
existing provider client. No 401 replay is introduced. External Codex auth is
read-only and reread each turn; expiry requires external renewal.

Pass a permission-enforcing HTTP transport (or the Host policy context consumed
by RemoteClient). Redirects are disabled for authenticated clients. Session-local
.env values no longer mutate process environment; configured proxy transport
belongs to the client. No authentication state belongs to a Session/Operation.

Validation: five real existing adapters with local SSE server and normalized
tool calls; request-time rotation; catalog and resume incompatibility; isolated
configuration; redacted errors; cancellation; redirect refusal and injected
transport denial. Real hosted inference is not needed for these contract tests.

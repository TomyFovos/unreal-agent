# Profiles

Select an explicit ID/version/source at Host composition. Resolve against the
declared provider/model family and persist Selection alongside provider.Selection
in Host metadata. ValidateResume rejects a changed version or identity. Child
requests use the same resolver. No substring model-name matching occurs.

Builtins:
- conservative v1: default guidance about observed content and revision checks.
- openai-reasoning v1: explicitly declared OpenAI reasoning family, with bounded
  read/edit/write guidance on existing tool descriptions.

Compose preserves the accepted tool set, names, schemas and Host lifecycle text.
It has no policy, credential, tool registration or session control APIs.

Compare can run identical prepared fixtures/model/settings through any existing
llm.Adapter and records its actual usage and normalized response for each profile.
Tests use a deterministic synthetic adapter to verify unchanged read-before-edit
behavior and comparison mechanics (100 input / 10 output synthetic tokens for
both). This is not evidence of a hosted model improvement. No live model
performance or token savings are claimed. BenchmarkCompose measures only local
composition cost; use Compare with an explicitly configured real adapter to
collect provider/model-specific behavior and token measurements.

Recorded local benchmark (Linux amd64, Go 1.27.1, Ryzen 5 5600X):
conservative 149.3 ns/op, 320 B/op, 2 allocs; openai-reasoning 346.2 ns/op,
640 B/op, 5 allocs. These measure composition only, not model effectiveness.

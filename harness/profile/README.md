# Profiles

Select an explicit ID/version/source at Host composition. Resolve against the
declared provider/model family and persist Selection alongside provider.Selection
in Host metadata. Child requests use the same resolver. No substring model-name matching occurs.

Builtins:
- conservative v1: default guidance about observed content and revision checks.
- openai-reasoning v1: explicitly declared OpenAI reasoning family, with bounded
  read/edit/write guidance on existing tool descriptions.

Compose preserves the accepted tool set, names, schemas and Host lifecycle text.
It has no policy, credential, tool registration or session control APIs.

Recorded local benchmark (Linux amd64, Go 1.27.1, Ryzen 5 5600X):
conservative 149.3 ns/op, 320 B/op, 2 allocs; openai-reasoning 346.2 ns/op,
640 B/op, 5 allocs. These measure composition only, not model effectiveness.

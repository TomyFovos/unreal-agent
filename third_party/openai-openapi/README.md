# OpenAI OpenAPI specification

This directory vendors the OpenAI OpenAPI specification from
[`openai/openai-openapi`](https://github.com/openai/openai-openapi).

- Revision: `dc708bbe9a149bc35132c567ef3a3fdd7a24ab49`
- Source: `https://raw.githubusercontent.com/openai/openai-openapi/dc708bbe9a149bc35132c567ef3a3fdd7a24ab49/openapi.yaml`
- SHA-256: `ab0c5306e390c64efbf50bbf71f02aa0dad2dafcaa96066a592186daa6103b87`
- Generator: [`g-logunov/oapi-codegen` at `0e050ab76086`](https://github.com/g-logunov/oapi-codegen/tree/0e050ab7608663a1ab9b243270f25181a4c88f22) (`v2.8.0` plus the nullable-union fix), pinned in [go.mod](../../internal/apigen/go.mod).

Requires Go 1.27+, Python 3.12+, and PyYAML 6.0.3. The generator tool and
its dependencies are pinned in a separate Go module; runtime dependencies and
the vendored OpenAPI file are unchanged. No `uv` dependency is required.
Use an isolated Python environment, then run from the repository root:

```sh
python3 -m venv /tmp/unreal-openapi-generator
/tmp/unreal-openapi-generator/bin/python -m pip install 'PyYAML==6.0.3'
PATH=/tmp/unreal-openapi-generator/bin:$PATH go generate ./internal/openaiapi
go test ./internal/openaiapi ./internal/apijson ./internal/jsonoptions ./harness/llm/responsesapi
/tmp/unreal-openapi-generator/bin/python -B -m unittest discover -s internal/openaiapi -p '*_test.py'
```

Generation rejects an unpinned PyYAML version before writing types. The checked-in
`types.gen.go` must match a fresh generation byte for byte. Discriminator mapping
uses actual wire values, removes ambiguous message-union mappings, and resolves
the pinned specification's recursive filter reference. No tool is registered or
executed by these generated wire types.

Schema patches live in [generate.py](../../internal/openaiapi/generate.py); the
vendored file remains unchanged. See [apijson](../../internal/apijson/README.md)
for serialization behavior and limitations of the generated types.

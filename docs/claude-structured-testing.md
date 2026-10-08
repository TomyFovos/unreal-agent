# Structured Bridge layered validation

Protocol debugging uses unit tests and synthetic SDK frames, followed by fake
CLI integration. Do not use Opus/AIdea E2E as an iterative debugger. Real
inference is excluded from ordinary tests, race tests and Make targets.

## Boundaries and test matrix

Canonical Session history, runtime, permissions, Operations and receipts remain
owned by Unreal. Refactoring introduces only disposable request projections and
an injectable generation boundary; it creates no new canonical state/executor.

| Layer | Independently tested boundary | Success | Failure |
| --- | --- | --- | --- |
| 1 Schema | `newActionSchema`, `compileStructuredDocument` | Draft 7; every Registry branch; Final; exactly one matching branch; immutable schema snapshot | Required/id/const/extra-property violations; invalid schema; unknown file/network ref; oversize |
| 2 Request | `buildStructuredRequest`, `structuredProtocolFrames`, `structuredPrompt` | Golden argv/env/initialize/user; model/effort; hooks `{}`; SDK MCP `[]`; private protocol instructions; Project Instructions snapshot | Missing isolation flag; parent credential/OTEL inheritance; invalid receipt kind/count; unbounded result; private replay/reasoning exclusion |
| 3 Initialize/control | `runStructuredProtocol` with in-memory reader/writer | Correlated success; optional response; empty pending lists; init before/after control; no user on capability probe | Wrong identity/type; denied initialization; pending permission/dialog; unexpected pre-init frame; incomplete EOF; no input on failure |
| 4 Assistant | `structuredAssistant` and SDK corpus | No subtype; nullable parent/stop; optional metadata; absent helper ID; opaque helper model | Missing/null/malformed message/content; unknown/execution metadata; wrong optional type; SDK errors |
| 5 Partial events | `readStructuredStream` with JSONL only | Start/delta/stop; fragmented serializer input; message_delta then message_stop; typed nullable metadata | Missing/wrong index; delta/stop without start; duplicate start; wrong partial type; size bound; incomplete blocks |
| 6 Serializer | StructuredOutput-only private normalization | Helper/result representations can differ; distinct serializer retries; correlated serializer result/progress/summary | Duplicate serializer ID in one content list; malformed helper; Bash/Read/Edit/Agent/Web/foreign MCP/unknown tools; uncorrelated results; deferred execution |
| 7 Structured result | `actionSchema.parse` directly (no stream) | Valid Action and Final | Missing/null/malformed/duplicate/trailing JSON; wrong envelope; unknown tool; wrong arguments; oversize |
| 8 Provider errors | `assistantFailure` | Exact classification of all 13 pinned enums | Non-SDK/case/whitespace/free text fail closed; no raw body/value retained; no proposal/public response |
| 9 Action envelope | `actionSchema.parse` | Exclusive Action or Final; valid id; known tool const | Both/neither branch; unknown fields; missing id/tool/arguments; invalid id |
| 10 Arguments | Registry-derived schema validators plus translators | Native/Bash/child schemas retained unchanged | Required/range/type/unknown-field violations; malformed arguments; no shell fallback |
| 11 Registry | Actual Registry lookup in Coordinator | read/glob/grep/write/edit/Bash | Unknown/unavailable tool; no fabricated Operation |
| 12 Permission | Existing Policy + Coordinator + executor | Explicit temporary-fixture grants | Tool deny: Operation 0; filesystem/process denial: typed failed Operation/receipt; unavailable sandbox stays denied |
| 13 Operation | Actual Coordinator/Manager/Native/Shell execution | Durable tool call before dispatch; ordered requested/terminal receipts; created/edited file | Failed match/command; denied access; cancelled operation; no direct provider execution |
| 14 Receipt/context | `structuredPrompt`, injected `RefreshContext`, shared Context Builder | Failed receipt reaches next round; dedup; bounded machine-owned output; transport reserve; Project Instructions retained | Invalid/mismatched receipt; runtime/schema drift; budget; private state/secret exclusion |
| 15 Final | Injected round controller + canonical integration | Final alone becomes public agent message | Provider/protocol failure produces no public response; helper JSON/prose/reasoning never public |
| 16 Replay/recovery | `runStructuredRounds` and canonical Coordinator recovery | Identical/equivalent JSON action replay reuses receipt; restart restores completed receipts | ID collision; uncertain operation requires recovery; max rounds; cancellation; no blind retry/fallback |

Schema, request, corpus, result and round tests never launch a Claude executable.
Registry/executor tests may execute explicitly permitted, harmless local commands
in temporary workspaces. They do not grant permissions to production Sessions.
Fake CLI/Host integration is a separate subsequent gate.

## Local pinned SDK corpus

The SSOT is the local `sdk.d.ts` paired with `sdk.mjs` Version **0.3.285** at
`/tmp/unreal-claude-discovery-sdk`; the installed CLI is 2.1.285. No other web
version is used. Tests carry synthetic, portable fixtures under
`harness/llm/clients/claudecode/testdata/structured/sdk-0.3.285/` and do not need
that `/tmp` directory, the SDK, an account or network at runtime.

The corpus covers SDKAssistantMessage, SDKPartialAssistantMessage, SDKUserMessage,
SDKControlResponse, SDKResultSuccess/Error and SDKAssistantMessageError, including
required content and optional/nullable/absent metadata. Dummy sentinel values
verify exclusion from errors/public responses. This isolated adapter supports a
deliberate subset: schema-valid SDK task/subagent/execution frames are still
forbidden. Private identity fields are not authorization or public replay state.

The CLI transport and authoritative validator are separate projections of the
same immutable Registry snapshot. The provider receives a closed, oneOf-free
transport object; Unreal retains the original strict Draft 7 union and tool
arguments schemas. Transport acceptance alone cannot create a proposal. The
schema/request golden snapshots are synthetic metadata, never credentials.

### Installed CLI `wire_tool_inputs` extension

The public 0.3.285 `sdk.d.ts` declares SDKAssistantMessage at line 3598 and does
not declare this internal field. The installed native bundle
`/home/e230038/.local/share/claude/versions/2.1.285` supplies the additional local
contract (line numbers refer to its embedded JavaScript):

- Line 1366368, byte 197560036: optional `wire_tool_inputs: record<string, unknown>`;
  the description identifies raw API tool inputs keyed by tool_use ID, separate
  from normalized message.content, for replay. This is not StructuredOutput-only.
- Line 1374071: capture/filter helpers retain object inputs associated with the
  message's tool_use IDs. The replay importer `kRt` starts at byte 203889525.
- Line 1380520, byte 210285912: serializer `Mo` publishes nonempty filtered inputs
  as the wrapper sibling `wire_tool_inputs`.

The initial SDK corpus followed public declarations and omitted this internal
wrapper extension. Synthetic fixtures now cover it; tests do not depend on the
local installation. Its raw inputs can contain model/user-controlled values and
are private transport data. The assistant parser validates only this exact
field, object inputs, IDs correlated with already checked StructuredOutput
blocks, resource bounds (1 MiB/64 entries/32 levels/4096 JSON values), and absence
of nested execution frames. It then discards the entire record. An Action-shaped
input is still opaque data, never an executable proposal.

The later message-array field was identified as input_transformations and is
covered below. Literal unknown_fields and arbitrary siblings remain forbidden.
Authoritative validation still starts from the final
result.structured_output and retains Registry/Permission/Operation boundaries.

Assistant/message metadata diagnostics report `metadata_scope=assistant|message`
and one actual member name only when it is 1..64 ASCII bytes from
`[A-Za-z0-9_.-]`. Empty,
overlong, non-ASCII and unsafe-punctuation names become `unknown`. This changes
diagnostics only: all unrecognized fields still reject at the same
serializer-frame boundary. The name is copied independently of the wire buffer;
no member value, raw JSON, tool input, prose or private ID is retained. Tests
cover multiple names, exact length boundaries, unsafe characters and sensitive
dummy values, including the safe name reaching a fake Host failure with zero
Operations and zero public responses.

### Installed message `input_transformations`

The local source is the same installed 2.1.285 native bundle. The pinned
`sdk.d.ts` imports BetaMessage at line 1 and uses it for SDKAssistantMessage at
line 3603; it does not explicitly declare input_transformations. The installed
implementation establishes these boundaries:

- Line 1370348, byte 201481881: the general Messages API stream accumulator copies
  non-null message_delta.input_transformations directly onto its message object.
  It is not tied to StructuredOutput. Message-start data also becomes the message
  object directly.
- Line 1375062, byte 204877072: `zFt` reads this field only as an array; missing or
  non-array values yield no transformation data.
- Line 1375062, byte 204877241: `qFt` checks entries with type=thinking_dropped,
  path:string and reason:string. It derives message/block indexes and maps binding
  mismatch reasons for private thinking bookkeeping. Adjacent consumers report
  input thinking drops and invalidate private thread-prefix state when necessary.

This is API input-transformation metadata transported through Claude, not an
Unreal tool request. Strings can be provider supplied or describe user-derived
input; none are public execution authority. The native consumer skips malformed
or other transformation entries. Unreal instead admits only the verified subset:
an absent field, empty array, or array of exact three-field thinking_dropped
objects with string path/reason. Present null, unknown types/fields, non-scalar
entries and execution-shaped objects fail closed. The stream accumulator's null
guard is not treated as proof of a nullable message-field contract.

The field is capped at 1 MiB and 64 entries. Fixed scalar entry fields bound
container depth to 2 and JSON values to 257; arbitrary nested objects/arrays are
forbidden. Path/reason values are never interpreted, indexed, replayed to another
provider or persisted. Metadata alone cannot produce any proposal: a missing
terminal result still fails. Only schema-validated result.structured_output can
reach Registry, Permission and a canonical Operation. Wire-input validation,
request/schema generation, provider errors and round-loop semantics are unchanged.

Synthetic tests reproduce the original scope=message/field=input_transformations
rejection before implementation. The retained corpus then verifies acceptance,
partial events and wire metadata coexistence, absence/empty/minimal entries,
invalid type/fields/nesting/count/size, private-value exclusion, unsafe tools and
unknown siblings. Fake Host tests require one canonical read despite replay,
zero raw metadata in history, and zero Operations/public responses on failure.

The operator confirmed Level 1 and variant B on real Opus, followed by failures
for C and C1 before serializer output. T1–T7 below then isolated a compatible
oneOf-free transport. Historical oneOf levels and AIdea E2E remain paused. A new
field or result failure goes back to its unit boundary; provider errors go back
to the provider boundary. Ordinary validation executes no real generation.

## Offline gates

```sh
make test-claude-structured-unit
make test-claude-structured
go test -race ./...
make check
make build
git diff --check
```

`test-claude-structured` depends on the unit gate before launching fake CLI
integration. It checks actual Host -> Context -> provider -> Registry -> Permission
-> Operation -> receipt -> next generation -> durable Final. Fixtures cover
read/glob/grep/edit/write, test fail -> reread/fix -> test pass, git status/diff,
duplicate proposals, both child providers, runtime pinning, cancellation,
restart/recovery, bounded Context and shared Orchestration projection.

For snapshot maintenance only, after reviewing a Registry/protocol change:

```sh
go test ./harness/llm/clients/claudecode \
  -run '^TestStructuredLayer(SchemaRegistryBranches|RequestSnapshot|ProbeSchemaLadder)$' \
  -args -update-structured-fixtures
```

This command executes no provider. Do not update snapshots to conceal a failure.

## Real adapter probe: explicit, last, separate from E2E

An explicitly authorized developer can run short adapter probes after their
focused offline/fake gate. Production adoption also requires the full offline
gates. This schema-only probe has no Host, Session, Operation Manager, workspace executor, child,
permission grant, Project Instructions loading or AIdea input. The CLI still
uses safe-mode, built-ins disabled, strict empty MCP, no persistence, restricted
environment, and StructuredOutput as the sole serialization-only helper.
The adapter's private temporary prompt file is cleaned normally.

Ordinary `go test` **skips** `TestStructuredRealAdapterSchemaProbe`; no environment
variable automatically enables it. Explicit probe flags authorize real model
generations. There is one generation per selected level and no
retry/provider/MCP fallback. No Action proposal is ever executed.

Three mutually exclusive opt-ins select the existing level/variant builders:

- `-claude-structured-probe-levels=N`: Level 1 through N, stopping on failure
- `-claude-structured-probe-exact-level=N`: only Level N, once; no earlier levels
- `-claude-structured-probe-variant=X`: only variant A–F/C1/C2/T1–T7, once; no schema levels

N must be 1–5. Level options default to zero and the variant defaults to empty
(real inference skipped). Conflicting selections are rejected before preflight
or subprocess launch. A variant cannot be combined with either level flag,
including an explicitly supplied zero. Exact and variant modes create
its own adapter, subprocess, pipes, parser and schema snapshot, with no prior
level or conversation continuation. It uses the same restricted tools/env and
five-minute generation timeout as ladder mode.

The offline probe audit uses the **same** per-level planner, controller and
outcome classifier as this opt-in test. A maximum level is only a loop bound;
it never enters the prompt, system instructions, initialization, model/effort,
environment or CLI options. Level 1 standalone and the five-level prefix have
the exact same 235-byte schema, SHA-256
`c703d71094a2f7c5555ae6d194a12293917421e44e6f4e930ffd86e9b1d9cfd9`.
Unit tests compare the complete request projection and actual initialize/user
wire bytes; native fake-CLI tests compare received argv, environment and prompt
files. Every exact level's request/schema/outcome matches its corresponding
ladder level; exact mode information never enters provider input. Only the
disposable temporary path/process identity is normalized, and absolute deadlines
are compared as their common five-minute duration.

Each level recompiles its own schema and starts/reaps a separate subprocess,
with a fresh parser, serializer tracking and five-minute deadline. Authentication
preflight and its restricted environment snapshot are shared read-only across
levels; no Session, resume option, conversation continuation or catalog lookup
is involved. Tests cover success/failure cleanup, repeated helper identities,
concurrent runs and a clean probe after an earlier failure. These tests run in
`make test-claude-structured`, with no real-provider opt-in flags.

The old `did not return the required validated Final` message combined separate
conditions. The new classifier emits only these closed outcomes:

- `validated_final` / `validated_action`: locally validated authoritative output
- `structured_missing` / `structured_null`: absent or null public output
- `schema_invalid` / `envelope_invalid`: local schema/registry/envelope rejection
- `provider_failure` / `protocol_failure`: typed upstream failure or invalid stream

Requests retain the existing Final-only prompt across both modes, allowing
schema complexity to vary independently of prompt content. A returned Final
still requires the exact requested message and empty public response output.
A schema-valid Final with different prose therefore reports
`probe_outcome=validated_final validation_reason=final_message_mismatch` and
fails the probe; this is distinct from a schema/parser failure. Level 1 permits
only Final. Levels 2–5 also accept a locally validated Action as evidence of
their schema branch, reporting `validated_action` without executing it. Level 2
permits only the inert validation witness `probe_proposal_only` with `{}`
arguments; it is not a Registry definition, exposed Claude tool or executor.
The generic wire schema is unchanged. Levels 3–5 require the existing Registry
tool/argument validation for their respective subsets. Unknown tools, invalid
arguments and all protocol failures remain rejected. No proposal becomes a
public response/ToolCall or creates an Operation.
Presence, schema-check booleans, envelope kind and existing closed stage/SDK
reason codes are safe to report. No proposal, prose, arguments, IDs or arbitrary
error string is retained in the probe report. A trailing bad event can report
`schema_valid=true` together with `protocol_failure`: earlier validation does
not override full-stream rejection.

| Level | Schema |
| --- | --- |
| 1 | Exact minimal Final-only object from the requested contract, without oneOf or registry |
| 2 | Final + generic inert Action envelope; one validation-only witness, no registered work tool |
| 3 | Final + actual Registry read schema |
| 4 | Final + actual Registry read/glob/grep schemas |
| 5 | Full checked Registry definition corpus: static tools plus parent/child definitions; actual Session exposure remains capability/permission dependent |

The operator confirmed a real Level 1 and variant B PASS. Exact Level 2,
variant C and single-branch C1 returned `provider_failure / assistant_unknown`
before any serializer/output validation. This identifies root oneOf as the
smallest observed compatibility boundary, not a universal provider prohibition
or a decoded remote error cause. The oneOf-free T probes below replace that
investigation path. Level 2–5, D–F and AIdea E2E remain paused. Ordinary tests,
Make targets and application startup never launch inference. An explicitly
authorized developer runs each selected real condition once; a diagnostic fix
must pass unit/fake tests before another condition is tried. No automatic retry
or fallback occurs. The probe reports
level/schema bytes/hash, closed outcome/reason
and validation booleans; no raw stdout, prose, arguments, IDs or policy body is
logged. `result.structured_output` remains authoritative and locally
schema-validated. Initialization/schema acknowledgement alone proves no backend
generation compatibility.

Identical offline requests and outcome fixtures exclude a level-count-dependent
harness difference. They do not establish the content of a past live response
or prove provider nondeterminism. Exact mode isolates each next schema test
without consuming another Level 1 generation.

An `assistant_unknown` result is a recognized upstream SDK error, not proof of
an Action parser failure. Request/schema rejection, output limits, overload and
server failure keep their distinct typed categories and closed reasons.

### Offline Level 1 → 2 schema audit

The audit reads the exact in-memory `schema.document` used by
`initialize.request.jsonSchema`. It does not write schemas or provider traffic
to disk. Both existing schema byte/hash contracts are unchanged:

- Level 1: 235 bytes, SHA-256
  `c703d71094a2f7c5555ae6d194a12293917421e44e6f4e930ffd86e9b1d9cfd9`.
- Level 2: 653 bytes, SHA-256
  `bcf0ceb61ffeb20ccdd18b854fb5152583ea51e16ebd0d8a778cee38941e2596`.

Level 2 first introduces `oneOf`, `minLength`, `maxLength` and `pattern`.
Its root has only `type: object` and `oneOf`; root properties, required fields
and `additionalProperties: false` live in the two branches instead. The Final
branch is semantically identical to Level 1. The Action branch has a `type`
const discriminator, an action object, bounded/patterned id, generic string tool
and closed object arguments. Neither a tool enum/const nor arguments properties
is present at this level. `const`, `required`, nested objects and
`additionalProperties: false` already exist in Level 1; their counts increase.
Neither level uses `anyOf`, `allOf`, `$ref`, `$defs` or `definitions`.

Schema-node counts increase 4 → 11, object-schema counts 2 → 6 and declared
property counts 3 → 8. The largest properties map grows 2 → 3. Schema-node
depth, including union branches, grows 3 → 4; output-object nesting grows 2 → 3
because Action arguments add an object. These small schemas do not approach
the installed strict derivation's 32-level/100,000-node bounds.

Local SSOT is `/home/e230038/.local/share/claude/versions/2.1.285`, an ELF with
embedded JavaScript. Byte offsets below are zero-based and specific to this
installed bundle; no other SDK or web version is substituted:

- Byte 195447625 (`Z3r`) / 195401816 (`replaceInitJsonSchema`): initialize stores
  the input schema; byte 221548242 calls this from the initialize handler.
- Byte 201241715 (`Ze`, reached through `p5e`): rejects excessive node/depth
  traversal and `$async`, then uses Ajv `validateSchema` and `compile` with
  `allErrors: true`, `validateFormats: false`. A rejected schema disables the
  structured serializer in the tool refresh path at byte 221411041. An
  initialize acknowledgement alone therefore does not prove generation support.
- Byte 201237013 (`qe`) / 201237283 (`Njn` and its walker): derives an optional
  **strict** input schema. Supported keywords are `$schema`, type, description,
  title, properties, required, additionalProperties, items, enum, const and
  anyOf. `oneOf`, `allOf`, refs/definitions and length/pattern constraints are
  outside this derivation. Objects must have a properties object; an explicit
  empty properties object is different from an absent one. Additional
  properties must be absent or false and the derived object is closed. Scalar
  const/enum are supported; anyOf is allowed only without sibling constraints
  other than annotations. The derived root must still be an object.
- Byte 201242559: the tool retains the original schema as `inputJSONSchema`,
  and attaches `strictInputJSONSchema` only when derivation succeeds. Local
  output validation continues to use the compiled original schema.
- Bytes 203870810 (`ymt`) / 203875249 (`FI`): the outgoing tool uses the original
  input schema unless structured-output capability and the strict feature gate
  select the derived one. Unsupported strict derivation falls back to the
  original **non-strict schema**, not another execution tool. No feature-gate
  value is inferred from offline source inspection.
- Byte 201240413 (`_e`): StructuredOutput validates/serializes its input and
  returns a structured-output attachment; this is not filesystem, process or
  child execution.

The pinned `/tmp/unreal-claude-discovery-sdk/sdk.d.ts` at lines 1968 and 4447
declares outputFormat/initialize.jsonSchema; line 3679 fixes the 13 assistant
error values. Its matching `sdk.mjs` identifies Version 0.3.285 at line 4,
stores jsonSchema unchanged at byte 1115700 and includes it in initialize at
byte 1128729. These interfaces do not promise that every Draft 7 schema is
accepted by the remote provider.

Offline evaluation of the extracted installed `Njn` walker, with ordinary
JSON-object dependencies and all schemas kept in memory, derives strict schemas
for variants A/B and reports `unsupported_keyword` for C–F. The portable Go
tests freeze the keyword/shape audit, not a second production normalizer.
**Strict incompatibility is confirmed; remote rejection is not.** The first
changed compatibility boundary is the root union/strict-to-non-strict tool
schema path. That is the next investigation target, while a generic provider
or CLI error remains possible.

### Text-only upstream error fixture and safe metadata

`probe-provider-unknown.jsonl` uses dummy values and reproduces the observed
assistant error envelope, text-only content, stop strings, nullable parent,
private IDs and timestamp. Unit and native fake-CLI tests fix its outcome to
`provider_failure`, `typed_failure`, `structured_result_invalid`,
`assistant_unknown`, with no serializer blocks, structured output, schema
validation, public response or Operation. No text is parsed as an Action.

The installed error factory `Vo` at embedded-JavaScript line 1376644,
byte 206905382 sets `isApiErrorMessage: true` on a text-only API-error message.
The serializer at line 1380520, byte 210296032 exports the optional top-level
boolean `is_api_error_message` only when that internal flag is true. The
embedded wrapper schema at line 1366368, byte 197560942 declares it as optional
and describes it as marking an assistant message that wraps an API error.
This is internal transport metadata, not an execution instruction. The
public 0.3.285 SDKAssistantMessage declaration at sdk.d.ts line 3598 omits this
CLI extension. The latest operator observation identifies this exact field
name in variant C; synthetic fixtures use dummy values. Neither raw response
text nor metadata values are retained, and no acceptance list is expanded.

An error can stop the parser at `error` before it examines a later metadata
key. Error projection now names one unrecognized assistant key under the same
bounded ASCII rule used for message keys, without replacing the original SDK
error or retaining its value. The diagnostic location is observational when
the provider error preceded metadata validation. The same boolean on an
otherwise successful serializer frame still fails closed as unknown metadata.

The CLI at bytes 203993661 / 203994384 maps otherwise unclassified API/local
errors to `error: unknown`. This does not identify unsupported_schema,
invalid_schema, complexity or a specific rejection. No such detail category is
invented, and error text/status values are not read into diagnostics or history.

### Probe-only intermediate schemas

All variants compile as Draft 7 and validate the fixed Final. A and F use the
unchanged Level 1/2 builders. B–E are in-memory mutations of those nodes; no
production schema or Action semantics change. Fake CLI tests see exactly the
same model/effort, prompts, initialization controls, argv/env, timeout and
session restrictions, with only jsonSchema changed. Each has a fresh generation
and no Action execution, MCP call or Operation.

| Variant | Addition | Bytes | SHA-256 |
| --- | --- | ---: | --- |
| A | Existing Final only | 235 | `c703d71094a2f7c5555ae6d194a12293917421e44e6f4e930ffd86e9b1d9cfd9` |
| B | Optional generic action object with an unconstrained id; no union | 329 | `a640f9b702ffed27b4be45e97e8f357c5d906d8809f8f7a4d9569da6b4382e82` |
| C | Exclusive Final/Action branches using root oneOf | 474 | `31818fd70e7f563717a8e857b36803dbf0b4e6d0ff4e4607bd93501b147b870a` |
| D | Generic string Action.tool | 499 | `7b5530638af483c115a4a65cabacdb453145ddeb2216ae5cb00b4e5d55c5f20e` |
| E | Closed empty Action.arguments schema | 558 | `6cdbe448806add436aa822a7b98a7a2eeb06708bf1a201df3828ecc6e77c5601` |
| F | Existing Level 2 id bounds/pattern and required action fields | 653 | `bcf0ceb61ffeb20ccdd18b854fb5152583ea51e16ebd0d8a778cee38941e2596` |

The exact 329-byte B schema still requires `type=final`
and `final`; a bare `type=action` is schema-invalid. In this schema-only probe,
a validated Final with the optional `action` object is an inert Action-branch
witness, reported as `validated_action`. A Final without that object reports
`validated_final`. Both still require the exact requested Final message. The
production mutually exclusive Action/Final envelope is unchanged and would
reject the combined shape. C–F can instead validate their explicit Action
branches, without Registry lookup or execution.

### B → C exact tree difference and oneOf micro variants

`TestStructuredLayerProbeMicroBCExactTreeDiff` freezes and enumerates **all**
canonical JSON pointer changes in memory: 21 removed paths, 33 added paths,
no changed scalar/container at a surviving path. Root type=object survives.
It is accurate to say that **oneOf is the only new schema keyword**, but the
tree and accepted values change in additional ways:

- Root properties, required=[type,final] and additionalProperties=false move
  into the Final branch. Its final/type property subtrees keep their values.
- B's optional action property is removed from the Final branch. C's first
  branch is precisely the Level 1 Final schema, without that optional property.
- A separate closed Action branch appears with type=object, type const=action,
  required=[type,action] and an action property. The minimal action object
  (id:string, no required id, closed properties) is copied unchanged from B.
- B's combined Final plus optional action is valid in B but invalid in C.
  C's standalone Action is valid in C but invalid in B. No tool, arguments,
  ID length/pattern, enum, reference, anyOf or allOf first appears in C.

The complete removed nodes are `/additionalProperties`, `/properties`,
`/required` (including `/0` and `/1`) and these property subtrees:

| Removed prefix | Every suffix, including the prefix itself |
| --- | --- |
| /properties/type | empty, /const |
| /properties/final | empty, /type, /additionalProperties, /properties, /properties/message, /properties/message/type, /required, /required/0 |
| /properties/action | empty, /type, /additionalProperties, /properties, /properties/id, /properties/id/type |

All added nodes are `/oneOf`, `/oneOf/0`, `/oneOf/1` and these branch subtrees:

| Added prefix | Every suffix |
| --- | --- |
| /oneOf/0 | /type, /additionalProperties, /properties, /required, /required/0, /required/1 |
| /oneOf/0/properties/type | empty, /const |
| /oneOf/0/properties/final | empty, /type, /additionalProperties, /properties, /properties/message, /properties/message/type, /required, /required/0 |
| /oneOf/1 | /type, /additionalProperties, /properties, /required, /required/0, /required/1 |
| /oneOf/1/properties/type | empty, /const |
| /oneOf/1/properties/action | empty, /type, /additionalProperties, /properties, /properties/id, /properties/id/type |

C1 and C2 use one shared probe-only builder for offline tests and the opt-in
real request. They preserve the already-tested root type=object to avoid
introducing an absent-root-type difference. The first branch embeds the exact
235-byte Level 1 Final schema **byte for byte**, without regenerating it.

| Variant | Root oneOf branches | Bytes | SHA-256 |
| --- | --- | ---: | --- |
| C1 | The exact Level 1 Final schema only | 263 | `1a413b1a6b5352afdc177df9d5d8e6240a5236f4d8f2f0a45cb0cdcaaedfc1f5` |
| C2 | C1 plus a minimum closed object requiring only type const=action | 371 | `214d3eb07424db8eb5009c0274fecab3e96e91380f84b639726a6e772f4766fb` |

C2 derives the second branch from C by removing its action property and that
required field. The only accepted witness for that branch is
`{"type":"action"}`: there is no ID, tool, arguments or executable Action
payload. It reports probe-only `validated_action`; the production parser still
rejects it. Both micro variants compile as Draft 7. Tests freeze hashes, exact
initialize.jsonSchema bytes, branch exclusivity and absence of new keywords
other than oneOf. They also compare complete shared-builder requests and
actual fake-CLI argv/env, model/effort, prompts, controls and session options.

Fake CLI cases cover C1 Final/provider/protocol failure and C2
Final/inert-Action/provider/protocol failure. Each generation has a fresh
subprocess, pipes, parser, private temporary directory and cleanup, including
a Final after earlier failures. Serializer partial events are shared unchanged.
Only StructuredOutput is permitted; unsafe built-ins, foreign/unknown tools
and trailing execution reject. API-error metadata continues to report
assistant_unknown and its safe field name without guessing unsupported_schema.

C1 subsequently failed live before any serializer block. OneOf presence is
therefore sufficient in that observed request; it does not prove a universal
provider keyword prohibition. C2 was not needed for the next transport design.

Variant output validation is test-only. It checks strict JSON (including
duplicate/trailing rejection), size and the selected compiled Draft 7 schema
against the original authoritative `result.structured_output`. To reuse the
unchanged execution-envelope stream parser for incomplete intermediate shapes,
the test reader projects only that field to a fixed inert Final. Every other
frame and field passes through the shared protocol/security parser, including
the complete tail. Invalid original output retains its typed rejection; a
protocol/provider/trailing-execution failure always prevents success. The inert
projection cannot produce an Action or ToolCall, and the harness returns only
safe outcome flags. No original output body, arguments or IDs enter reports,
canonical history, Registry, Permission or Operations.

The variant and offline requests have identical schema bytes, prompts, system
instructions, initialize/user frames, argv/env, model/effort, tool restrictions,
timeout and session settings. Fake CLI tests verify Final, inert Action,
provider failure, protocol failure, fresh processes and cleanup after failure.
There is no automatic retry or advancement. C1's live failure is retained as
diagnostic history; the next investigation uses the oneOf-free transport below.
The safe report includes schema bytes/hash, the eight outcome categories,
output presence/null, schema-check flags, envelope kind, validation reason and
typed provider/stage/metadata-name diagnostics. Schema probes always keep
execution, MCP and Operations at zero. A failure returns to an offline fixture.

```sh
go test ./harness/llm/clients/claudecode \
  -run '^TestStructuredLayerProbe(Micro|LevelOneTwoExactSchemaDiff|OfflineSchemaVariants|VariantRequestsChangeOnlySchema|ObservedUnknownProviderFailure|ProviderFailureMetadataNames|APIMetadataIsNotNewAcceptance)' \
  -count=1 -v
```

## OneOf-free transport gates T1–T7

The original strict Registry schema remains byte-identical. The provider-facing
schema is a closed serialization superset generated from that immutable
snapshot. Unreal checks the result again against the strict schema, tool lookup
and the selected tool's arguments, including undeclared argument names, before
producing a ToolCall or Final. Semantic constraints are never delegated to
transport sampling. The private per-tool contract is reserved in the bounded
Context budget together with framing and the latest canonical receipt.

| Variant | Incremental transport addition | Bytes | SHA-256 |
| --- | --- | ---: | --- |
| T1 | Single closed object, optional Final/Action | 279 | `addf8d177956663819cd4b0e76e96c15e90082800fa69f8e2cb5e0a8ce0c3516` |
| T2 | Type discriminator enum | 305 | `bf51936c706404535bec4aa6a711fb0e3224bfde9e43bd9a44a40f1d73dee564` |
| T3 | Action id string | 327 | `ec55f6c4c4c0f87bce954275cb0fa90c6dcc9c852c3c8ad7b2909d9539113a71` |
| T4 | Action tool string | 352 | `9e12c61e7ea6bbd9636b452e23d23f7ad01a06fd25a8550d8241a0b12f13a320` |
| T5 | Closed arguments object | 427 | `8a06c10b2adae8f96f5074f491eb2c061c7a166a8fb8c13d163dff89ce8343c6` |
| T6 | Required discriminator/action fields | 484 | `e86b3319e668869f8213f7ca0a18c38f50773933fd99108a2cc6eb525e6358c4` |
| T7 | Registry-derived tool enum and closed argument-property superset | 1910 | `189b395f91c1f77b46c5846c7c0be7725f65030cec11f6226855176d82001e80` |

Unit tests freeze bytes/hashes, Draft 7 compilation, supported CLI keywords,
offline/request equivalence and all current Registry Action witnesses. Fake
CLI tests verify isolated generations and exact initialize data. T7 additionally
validates the original result with the production semantic validator; the
probe's inert projection cannot bypass it. Ordinary tests do not execute Action
proposals. No union, reference, pattern or numeric/string range is sent to the
provider; those constraints remain authoritative in Unreal.

In this development run all seven variants passed real Claude Code 2.1.285 /
Team / Opus / high with validated Final, zero MCP and zero Operations. T3's
first response was schema-valid Final with a different fixed message, reported
as final_message_mismatch rather than a schema failure. A synthetic fixture
retained that failure assertion. The probe instruction was made an explicit
JSON literal, and one T3 probe with the changed instruction passed; T4–T7 used
the same literal. No raw response was logged and no assertion was relaxed.

For an explicitly authorized schema-only diagnostic, select exactly one T
variant with the existing opt-in harness, for example:

```sh
go test ./harness/llm/clients/claudecode \
  -run '^TestStructuredRealAdapterSchemaProbe$' -count=1 -v -timeout=7m \
  -args -claude-structured-probe-variant=T7 \
  -claude-structured-probe-binary=/home/e230038/.local/bin/claude \
  -claude-structured-probe-model=opus
```

Production supports the current Registry's scalar, array and closed-object
arguments. Open dictionaries, argument unions/references, missing declared
properties or incompatible types for the same property across tools fail
construction. Unsupported shapes require an explicit future transport design;
there is no schema widening or protocol fallback.

### Separate real Action/receipt/Final gate

`TestClaudeStructuredRealAdapterRoundProbe` is skipped unless explicitly enabled.
It creates a private temporary workspace/config/Host/state/socket. The only
Unreal executor permission is read within that workspace; normal default
permissions and existing Sessions are unchanged. A dummy marker is stored only
in probe.txt, absent from the user request. The first generation must request
one read; Unreal records and executes the canonical Operation. The next fresh
generation must return a Final containing exactly that receipt's marker.
The test requires one completed Operation, two canonical model responses,
bounded Context, no provider protocol in history and no fixture modification.
The Host and subprocesses drain before temporary state is removed.

The same harness runs against a native fake CLI in ordinary validation. Its
configuration unit distinguishes static model reasoning capability from the
runtime bridge tool capability and checks that shell/process permission remains
denied. The real Team / Opus / high round gate passed in this development run:
one read Operation, two generations, exact receipt-derived Final, zero MCP,
zero Claude side-effecting built-ins and zero workspace changes.

After the full offline gate, an explicitly authorized developer can run it:

```sh
go test ./cmd/unreal-agent \
  -run '^TestClaudeStructuredRealAdapterRoundProbe$' -count=1 -v -timeout=7m \
  -args -claude-structured-real-round-probe \
  -claude-structured-real-binary=/home/e230038/.local/bin/claude \
  -claude-structured-real-model=opus
```

No AIdea or normal state path participates. These flags never enable inference
in ordinary tests, race tests, Make targets, bootstrap or preflight.

## AIdea gate and limitations

Only after the offline suite, compatible T1–T7 transport probes and real
Action/receipt/Final gate succeed is the implementation ready to stop for the
AIdea E2E decision. The old incompatible oneOf ladder is not rerun. A future
separately authorized read-only AIdea E2E would be glob -> read AGENTS.md ->
read README.md -> Final. Keep history, reviewed permissions and Host lifecycle.
Do not edit AIdea, enable side-effecting Claude built-ins, run E2E after each fix,
or collect raw private streams. A failed probe returns work to its unit fixture.

Offline tests establish adapter/Unreal correctness for the matrix, not live
account entitlement or provider availability. Real probe outcomes are deliberately
not recorded as passing by the offline gate. Provider-specific private state is
never required for replay; uncertain side effects still need canonical recovery.

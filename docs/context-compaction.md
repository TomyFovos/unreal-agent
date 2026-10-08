# Context Compaction v1

Session history is the complete, append-only source of truth. Provider context
is a temporary selection from that history. Compaction never edits or deletes
canonical records, and does not append generated summaries or synthetic turns.

v1 runs locally and deterministically. It requires no additional model, local
LLM, compaction service, embeddings, vector database or external retrieval
service. The existing Claude/Codex authentication boundaries are unchanged.

## Previous request construction

`contextbuilder.Builder` retained one system message (harness preamble, runtime
prompt, creation-time Project Instructions and applicable skills), the entire
committed conversation and staged inputs/results. Every `Build` copied those
items into the next request. There was no input budget, historical retrieval or
active compaction algorithm. `TurnCompaction` existed as a history-codec type,
not an implemented compaction strategy.

Public user messages, quoted typed peer messages, model messages/tool calls,
and Translator-generated receipts were replayed from Session Store. Receipts
are reconstructed by the existing Translator from canonical tool-call status
and versioned Operation snapshots. Existing executor truncation/capture-file
behavior predates this feature; compaction cannot restore bytes never recorded.

Codex and Claude already shared this builder. Claude/foreign-provider history
used portable quoted messages without private reasoning or continuation IDs.
The same-provider Codex native tool loop retained its native call/result and
provider replay data. Runtime changes applied only at the existing turn boundary.
Resume reconstructed the builder through paged canonical replay. Previously,
`/analyze Context` displayed observed response input usage, not request selection.

## Four layers

```text
Canonical Session History (unchanged)
    -> eligible provider-neutral Context Units
    -> deterministic selector + local lexical retriever
    -> token-budgeted, request-local Context Package
    -> existing provider adapter
```

The engine is in `harness/contextengine`. `contextbuilder` supplies public units
and performs the final provider rendering. Host owns only disposable diagnostics
and a private derived manifest. Neither selection nor analysis adds canonical
memory or changes Session/Operation/Permission/RuntimeSelection ownership.

### Units and preservation

A unit has an insertion ID, kind, preservation class, canonical source reference,
turn/operation/child identifiers when known, canonical timestamp, provider origin,
tool group, exact public payload, estimated cost and content fingerprint.
Large text parts additionally have a parent unit ID and exact byte range.

| Class | Request behavior |
| --- | --- |
| `PIN` | Protect required current inputs/instructions/task; prioritize failure evidence. |
| `KEEP` | Include original public data when it fits. |
| `REFERENCE` | Include a source reference while retaining the original for retrieval. |
| `OMIT_FROM_REQUEST` | Exclude this request projection; canonical history remains intact. |

Units cover user/model messages, tool requests/results, typed child/peer results
and public model/tool failure evidence. Instructions and structured state are
separate protected parts of the package rather than duplicate canonical records.
Canonical control/configuration records contribute only allowlisted metadata.

The latest explicit user request remains pinned across a long response/tool loop.
A child separately pins the initial delegated task input identified by its existing
canonical ChildConfig. A new explicit user input replaces the previous current
request binding; this does not infer that older user constraints were resolved.
Older original constraints remain retrievable.

### Structured state and checkpoints

Structured state contains provider/model/effort/revision, tool capability,
Unreal-enforced permission constraints, Project Instructions metadata, source
boundary, current request/task references and observed Operation/child metadata.
Active/failed Operation and observed-child counts are exact for the replayed
state. Detail rows are bounded; omitted details are counted as referenced state.
No task decision, changed file, test outcome or blocker is guessed from prose.

A checkpoint has schema version, canonical source range and `ThroughSequence`,
the structured machine state at that boundary, retained source references and
the canonical record timestamp. It is not a summary of the older conversation.
The package combines its checkpoint reference with current machine state,
recent original data, retrieved original evidence and current input. Recent raw
selection may cross the checkpoint boundary to preserve a fitting interaction.

Project Instructions continue to use the original Session snapshot. Resume and
children never substitute a fresh disk read. Parent history is not copied into a
child: it receives its task, inherited instructions and its own history.

### Retrieval and provenance

The default backend is an incremental, in-memory inverted lexical index.
Queries use the current explicit user request, with the explicitly bound child
task as a fallback. Unrelated recent questions are not added to the query.
There is no model query rewrite. Terms preserve filenames, symbols, error codes
and their components; Han bigrams support CJK matching. Explicit code/path terms
have increased weight. Exact code/path/identifier matches precede ordinary words.
For the same identifier match, the earliest original source precedes later
questions repeating it; parts of that source are ranked by matched context.
Equal scores use stable insertion IDs. This is lexical ranking, not an inference
that a source contains an answer. Selection does not depend on wall-clock time or
Go map iteration.

Each result keeps history sequence, turn, operation/child identifiers and
timestamp when available. Large messages/receipts use 1024-byte UTF-8-safe source
parts with 128-byte overlap. Retrieval uses the original text, not paraphrases.
Current/staged input, the current turn and actually selected recent raw sources
are excluded before ranking limits are applied. A recent candidate that did not
fit remains searchable. Whole-source, repeated-content and overlapping-part
duplication is removed before consuming retrieval slots or budget. Retrieved evidence is
visibly quoted as historical data, not promoted to a new instruction.

Successful large tool receipts normally become source references. Their exact
eligible text parts remain searchable. Recent failures are prioritized; large
failures retain fitting first/last original parts and can retrieve the rest.
Existing canonical Operation evidence determines failure state; v1 does not
guess that a later conversation resolved it.

`HistoryRef` and `Engine.ResolveUnit` provide the internal source/recall boundary.
No model-facing recall tool is added. `Retriever` and `Selector` interfaces allow
future FTS/BM25/embedding/hybrid or model-assisted strategies without rewriting
canonical history.

## Budget and rendering

For each selected provider/model:

```text
input budget = min(configured InputBudget,
                   context window - ResponseReserve
                                  - actual schema estimate
                                  - ProtocolReserve)
```

Trusted context-window metadata already recorded in RuntimeSelection is used
when available. An unknown window uses the configurable conservative fallback;
the engine does not invent a provider window or discover a catalog every turn.
An initial selection without recorded catalog window metadata also uses fallback.

Costs use JSON/UTF-8 byte counts plus framing margins, rather than an optimistic
characters/4 conversion. These are estimates, not actual token measurements.
Instructions/current input/delegated task are never silently truncated. If they
cannot fit, a safe typed `context: required_context_exceeds_budget` failure
occurs before inference. Configure an appropriate budget for unusually large
instructions/input. Response reserves are planning reserves, not a guarantee of
the backend's eventual output length. Image accounting is deliberately conservative;
a current native image is delivered intact or fails budget validation.

Priority is required instructions/current input/task and structured state, fitting
failure evidence, a reserved recent raw suffix, retrieved historical evidence,
then additional fitting recent data. Optional Operation detail cannot crowd out
required input. Exact estimates are checked again after final rendering.

`RetrievalReserve` protects a deterministic historical-evidence slice from a
large `RecentReserve`. Its zero/default value is `min(2048, InputBudget/4)`;
the reserved slice is capped at half the budget remaining after pins. Recent
selection uses the remainder of that first pass, retrieval uses fitting candidates,
and unused space is then filled by additional recent data. Required input and
instructions are never evicted. An oversized source can use original indexed
parts; evidence that still cannot fit is counted as skipped-budget, not delivered.

Claude receives ordinary text context and quoted historical tool receipts with
`Request.Tools=[]`. Safe-mode, disabled built-in tools, strict empty MCP, no Claude
session persistence and execution-event rejection remain unchanged.

Codex retains its selected native tool schemas, Translator and Operation path.
Fitting same-provider call/result groups remain native; incomplete groups become
quoted historical evidence. A large result may use a native result containing a
source reference. Provider-neutral packages/index/checkpoints never contain
private reasoning or provider replay IDs. The existing final Codex renderer may
replay selected same-provider native reasoning/state, budgeted separately in
transient provider-local memory; it cannot cross into Claude/another provider.

Provider switching rebuilds the package using the new budget/capability at the
existing `runtime_selection -> runtime_applied` boundary. It never redirects an
in-flight request, rewrites earlier turns, or performs automatic fallback.

## Configuration and comparison

Existing config files need no changes: bounded v1 is the default. Optional
top-level `Context` uses the same JSON field style as the runtime configuration:

```json
{
  "Context": {
    "Version": 1,
    "Mode": "bounded",
    "FallbackWindow": 32768,
    "InputBudget": 24576,
    "ResponseReserve": 4096,
    "ProtocolReserve": 1024,
    "RecentReserve": 12000,
    "RetrievalReserve": 2048,
    "RetrievalLimit": 8,
    "CheckpointThreshold": 128,
    "LargeToolTokens": 2048
  }
}
```

This is an addition to a complete existing runtime JSON, not a standalone
configuration. Zero/omitted fields select the defaults above. Unknown versions,
modes, negative values and oversized limits fail validation. Threshold counts
canonical record sequences, not conversation turns. Schema reserve is computed
from the selected request's actual tool definitions; Claude text-only reserves
no tool schemas. Units/counts can include additional source parts.

`Mode: "legacy"` is an explicit comparison option for the previous unbounded
selection, with the same public-data security boundary. It is not a second
canonical memory implementation. Context strategy is outside immutable runtime
creation identity, so old Sessions can resume without history migration.
An existing healthy Host retains its startup config until restarted.

Host and standalone runner enable the same engine. Actual child processes also
enable bounded v1; children currently use the conservative default strategy
parameters rather than inheriting custom parent Context tuning.

## Derived files, resume and crash safety

```text
<session-directory>/context-v1/<session-ID>/derived.json
```

Directories are private 0700; the regular manifest is 0600. A pinned directory,
temporary file, fsync and atomic rename isolate updates from canonical files.
Unsafe/symlink/public derived locations disable persistence rather than weaken
permissions or corrupt the Session. The engine can continue in memory.

Only versioned checkpoint source references/structured metadata and body-free
diagnostics are persisted. Original bodies, lexical terms and private replay data
are not duplicated into derived files. The live lexical index is rebuilt from
canonical replay on restart, even when a manifest is readable. Missing, stale,
incompatible, corrupt and partially written manifests cannot change selection or
make canonical history corrupt. A checkpoint/index can be discarded and rebuilt.

The manifest is advisory and not a startup acceleration mechanism in v1.
Request diagnostics are ephemeral: after restart they are `unknown` until the
next request is prepared, instead of reusing a possibly stale previous report.
Preparing/rebuilding context alone does not send an inference request. Existing
resume semantics still execute a genuinely pending canonical input.

## Security and analysis

Eligibility accepts only public message/tool payload types and known public
message phases. Reasoning, raw usage/provider metadata, credential controls,
authentication/environment/configuration bodies and account identity have no
index ingestion path. Known secret patterns exclude a whole source unit rather
than rewriting it into apparently original evidence. Credential/environment read
requests are ineligible, and exclusion propagates to their receipts even if the
result has no recognizable label. Required sensitive input produces a closed
error instead of sending it. Free-form content screening is conservative pattern
matching, not a complete detector for arbitrary unlabelled secrets.

`/analyze` -> Context (or `/analyze context`) reports last prepared package:
provider/model/runtime revision, projected unit count, raw/pinned selection,
retrieval/reference/omission/exclusion counts, checkpoint/source boundary,
estimated input and budget/utilization, reserve/window source, retrieval latency
and cache status. `Retrieved` is the number of historical units actually present
in the final Context Package. Separate diagnostics count eligible candidates and
skips for selected recent data, duplicates, budget and ineligible sources. These
are counts, not returned bodies or search terms. It displays no bodies, queries or raw tool arguments. Existing
actual provider input-usage charts remain separate, with known/partial/unknown
semantics. JSON/Markdown analysis exports include only this safe metadata.
Tiny/Narrow/Wide, NO_COLOR/plain/ASCII and the bottom composer are unchanged.

## Performance and limits

Units, lexical postings, fingerprints and rendering costs are incrementally
updated for new records. Each request scans a budget-bounded recent suffix, not
all historical bodies. Checkpoint references accumulate incrementally. Memory is
proportional to eligible history/index size; v1 does not evict that local index.
Queries on common terms can still scan/sort O(N) matching postings. Operation
state selection scans lightweight stored metadata. Restart pays canonical replay
cost, including the existing store's page-reading behavior.

Tests cover 500/1000 turns, exact old codes/files/symbols/constraints/child results,
provider switches, native tool groups, large receipts, active task/failure
retention, budgeting, secrets, determinism, cache deletion/corruption/version
changes, real Host/gateway composition and actual fake child processes.
Lexical matching cannot infer semantic relevance, remember a decision not
explicitly recalled, or recover output not present in canonical evidence.
No generated summary, monetary/quota estimate, embedding or recall tool is added.

## Manual smoke: user execution only

Use the same existing config, Session name, state and socket as your long Session.
No smoke command below is run by the implementation/tests. Detach, stop the
confirmed owning serve process with SIGTERM, wait for it to exit, and use the
updated binary. Do not remove history, socket locks or canonical state.

For the existing multi-provider Team smoke store:

```bash
cd /home/e230038/src/unreal-agent
./bin/unreal --config "$HOME/.config/unreal-agent/multi-provider-main-smoke.json" \
  --state-directory "$HOME/.local/state/unreal-agent-claude-team-smoke" \
  --socket "$HOME/.local/state/unreal-agent-claude-team-smoke/run/host.sock" \
  claude-team-smoke
```

Use your actual long Session/config if it has a different name or directory;
keep immutable runtime/policy/template values unchanged. Context defaults apply
without changing the runtime JSON. A stopped Session resumes automatically;
running Sessions attach. A pending real input can resume inference as before.

1. Open `/analyze context`. Before the first newly prepared request, package
   diagnostics correctly show unknown; existing measured usage remains visible.
2. Use `/model` to select Claude -> model -> effort. Ask for the exact wording of
   an old error code, file/symbol, constraint or child result unique to this Session.
3. Inspect `/analyze context`: retrieved count/source boundary, estimated input
   within budget, reference/omission counts. It should not grow with full history.
4. Switch to Codex -> model -> effort and repeat; then reverse the switch. The
   history is retained and only public selected context crosses providers.
5. Ctrl-D detach and reopen the identical launcher command. After Host restart,
   request original evidence again and compare the safe context metrics.
6. To force derived rebuild, stop the owning Host after an idle stop/detach, and
   remove **only** the following derived manifest. Keep every `.session.jsonl`
   and canonical Operation/history file. Reopen the same command and ask again.

```bash
rm -- "$HOME/.local/state/unreal-agent-claude-team-smoke/sessions/context-v1/claude-team-smoke/derived.json"
```

For another store replace only the session-directory and Session ID in this
derived path. Rebuild does not rewrite history; new user turns append normally.

未来の selector/retrieval の変更も derived state の再構築だけで適用できます。
canonical history は削除していません。生成 summary は canonical memory ではありません。

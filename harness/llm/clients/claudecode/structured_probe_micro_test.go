//go:build linux || darwin

package claudecode

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

const (
	structuredProbeC1Hash    = "1a413b1a6b5352afdc177df9d5d8e6240a5236f4d8f2f0a45cb0cdcaaedfc1f5"
	structuredProbeC2Hash    = "214d3eb07424db8eb5009c0274fecab3e96e91380f84b639726a6e772f4766fb"
	structuredProbeC2Witness = `{"type":"action"}`
)

// Compare every canonical tree node without persisting either schema. Container
// descriptions represent only their JSON type: added/removed child paths cover
// membership changes separately. Scalars include their exact schema value.
func probeSchemaTree(t *testing.T, raw jsontext.Value) map[string]string {
	t.Helper()
	tree := map[string]string{}
	var walk func(string, any)
	walk = func(path string, value any) {
		switch value := value.(type) {
		case map[string]any:
			tree[path] = "object"
			for _, key := range slices.Sorted(maps.Keys(value)) {
				escaped := strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
				walk(path+"/"+escaped, value[key])
			}
		case []any:
			tree[path] = "array"
			for index, child := range value {
				walk(fmt.Sprintf("%s/%d", path, index), child)
			}
		default:
			encoded, err := json.Marshal(value)
			if err != nil {
				t.Fatal("schema node encoding failed")
			}
			tree[path] = string(encoded)
		}
	}
	walk("", probeSchemaObject(t, raw))
	return tree
}

func TestStructuredLayerProbeMicroBCExactTreeDiff(t *testing.T) {
	variants := probeSchemaVariants(t)
	b, c := variants[1].Schema, variants[2].Schema
	before, after := probeSchemaTree(t, b.document), probeSchemaTree(t, c.document)
	var removed, added, modified []string
	for path, value := range before {
		if next, found := after[path]; !found {
			removed = append(removed, path)
		} else if value != next {
			modified = append(modified, path)
		}
	}
	for path := range after {
		if _, found := before[path]; !found {
			added = append(added, path)
		}
	}
	slices.Sort(removed)
	slices.Sort(added)
	wantRemoved := strings.Fields(`
/additionalProperties
/properties
/properties/action
/properties/action/additionalProperties
/properties/action/properties
/properties/action/properties/id
/properties/action/properties/id/type
/properties/action/type
/properties/final
/properties/final/additionalProperties
/properties/final/properties
/properties/final/properties/message
/properties/final/properties/message/type
/properties/final/required
/properties/final/required/0
/properties/final/type
/properties/type
/properties/type/const
/required
/required/0
/required/1`)
	wantAdded := strings.Fields(`
/oneOf
/oneOf/0
/oneOf/0/additionalProperties
/oneOf/0/properties
/oneOf/0/properties/final
/oneOf/0/properties/final/additionalProperties
/oneOf/0/properties/final/properties
/oneOf/0/properties/final/properties/message
/oneOf/0/properties/final/properties/message/type
/oneOf/0/properties/final/required
/oneOf/0/properties/final/required/0
/oneOf/0/properties/final/type
/oneOf/0/properties/type
/oneOf/0/properties/type/const
/oneOf/0/required
/oneOf/0/required/0
/oneOf/0/required/1
/oneOf/0/type
/oneOf/1
/oneOf/1/additionalProperties
/oneOf/1/properties
/oneOf/1/properties/action
/oneOf/1/properties/action/additionalProperties
/oneOf/1/properties/action/properties
/oneOf/1/properties/action/properties/id
/oneOf/1/properties/action/properties/id/type
/oneOf/1/properties/action/type
/oneOf/1/properties/type
/oneOf/1/properties/type/const
/oneOf/1/required
/oneOf/1/required/0
/oneOf/1/required/1
/oneOf/1/type`)
	if !slices.Equal(removed, wantRemoved) || !slices.Equal(added, wantAdded) || len(modified) != 0 ||
		len(before) != 23 || len(after) != 35 {
		t.Fatal("B -> C exact tree difference changed", removed, added, modified)
	}
	for _, path := range removed {
		t.Logf("removed %s (%s)", path, before[path])
	}
	for _, path := range added {
		t.Logf("added %s (%s)", path, after[path])
	}
	bInspection, cInspection := inspectProbeSchema(t, b.document), inspectProbeSchema(t, c.document)
	var keywords []string
	for keyword := range cInspection.Keywords {
		if bInspection.Keywords[keyword] == 0 {
			keywords = append(keywords, keyword)
		}
	}
	if !slices.Equal(keywords, []string{"oneOf"}) {
		t.Fatal("oneOf is not the sole newly introduced keyword")
	}
	root := probeSchemaObject(t, c.document)
	branches := root["oneOf"].([]any)
	if !reflect.DeepEqual(branches[0], probeSchemaObject(t, variants[0].Schema.document)) ||
		!reflect.DeepEqual(branches[1].(map[string]any)["properties"].(map[string]any)["action"],
			probeSchemaObject(t, b.document)["properties"].(map[string]any)["action"]) {
		t.Fatal("Final or minimal action nodes changed while moving into branches")
	}
	for _, tc := range []struct {
		value string
		b, c  bool
	}{
		{structuredProbeFinal, true, true},
		{structuredProbeVariantBWitness, true, false},
		{probeVariantWitness(t, "C"), false, true},
		{structuredProbeC2Witness, false, false},
	} {
		value, err := jsonschema.UnmarshalJSON(strings.NewReader(tc.value))
		if err != nil || (b.validator.Validate(value) == nil) != tc.b || (c.validator.Validate(value) == nil) != tc.c {
			t.Fatal("B/C union semantic difference changed")
		}
	}
}

func TestStructuredLayerProbeMicroContracts(t *testing.T) {
	final := structuredProbeSchema(t, 1)
	for _, variant := range probeMicroSchemaVariants(t) {
		t.Run(variant.Name, func(t *testing.T) {
			wantBytes, wantHash, wantBranches := 263, structuredProbeC1Hash, 1
			if variant.Name == "C2" {
				wantBytes, wantHash, wantBranches = 371, structuredProbeC2Hash, 2
			}
			if len(variant.Schema.document) != wantBytes || fmt.Sprintf("%x", sha256.Sum256(variant.Schema.document)) != wantHash ||
				variant.Schema.validator.DraftVersion != 7 || len(variant.Schema.tools) != 0 || len(variant.Schema.arguments) != 0 {
				t.Fatal("micro schema byte/hash/Draft 7/no-execution contract changed")
			}
			var root map[string]jsontext.Value
			var branches []jsontext.Value
			if json.Unmarshal(variant.Schema.document, &root) != nil || json.Unmarshal(root["oneOf"], &branches) != nil ||
				!slices.Equal(slices.Sorted(maps.Keys(root)), []string{"oneOf", "type"}) ||
				len(branches) != wantBranches || !bytes.Equal(branches[0], final.document) || string(root["type"]) != `"object"` {
				t.Fatal("micro Final branch is not the exact unchanged 235-byte Level 1 schema")
			}
			if wantBranches == 2 {
				tag := probeSchemaObject(t, branches[1])
				if !reflect.DeepEqual(tag, map[string]any{"type": "object", "properties": map[string]any{"type": map[string]any{"const": "action"}}, "required": []any{"type"}, "additionalProperties": false}) {
					t.Fatal("C2 second branch is not the minimum closed object with a distinct type tag")
				}
			}
			inspection := inspectProbeSchema(t, variant.Schema.document)
			finalInspection := inspectProbeSchema(t, final.document)
			for keyword := range inspection.Keywords {
				if finalInspection.Keywords[keyword] == 0 && keyword != "oneOf" {
					t.Fatal("micro variant introduced another untested keyword")
				}
			}
			for _, tc := range []struct {
				value string
				valid bool
			}{
				{structuredProbeFinal, true},
				{structuredProbeC2Witness, wantBranches == 2},
				{structuredProbeVariantBWitness, false},
				{`{"type":"action","action":{}}`, false},
				{`{"type":"action","tool":"Bash","arguments":{}}`, false},
				{`{"type":"final"}`, false},
				{`{"type":"other"}`, false},
				{`{}`, false},
			} {
				value, err := jsonschema.UnmarshalJSON(strings.NewReader(tc.value))
				if err != nil || (variant.Schema.validator.Validate(value) == nil) != tc.valid {
					t.Fatal("micro schema exclusivity/closed objects changed")
				}
			}
			t.Logf("variant=%s schema_bytes=%d schema_sha256=%s Draft7=PASS FinalBranch=byte-identical", variant.Name, wantBytes, wantHash)
		})
	}
}

func TestStructuredLayerProbeMicroSelection(t *testing.T) {
	for _, name := range []string{"C1", "C2"} {
		selection, err := selectStructuredProbe(0, 0, name)
		if err != nil || selection.Variant != name || len(selection.Levels) != 0 {
			t.Fatal("micro selector does not select exactly one variant")
		}
		for _, conflict := range []string{"-claude-structured-probe-levels=0", "-claude-structured-probe-levels=5", "-claude-structured-probe-exact-level=0", "-claude-structured-probe-exact-level=2"} {
			flags := flag.NewFlagSet("offline-micro-selection", flag.ContinueOnError)
			ladder := flags.Int("claude-structured-probe-levels", 0, "")
			exact := flags.Int("claude-structured-probe-exact-level", 0, "")
			variant := flags.String("claude-structured-probe-variant", "", "")
			if flags.Parse([]string{"-claude-structured-probe-variant=" + name, conflict}) != nil {
				t.Fatal("offline flag parsing failed")
			}
			if _, err := structuredProbeSelectionFromFlags(flags, *ladder, *exact, *variant); err == nil {
				t.Fatal("micro selection bypassed mutually exclusive probe flags")
			}
		}
	}
	for _, name := range []string{"C0", "C3", "C01", "c1", "C1 private-probe-input"} {
		selection, err := selectStructuredProbe(0, 0, name)
		if err == nil || selection.Variant != "" || len(selection.Levels) != 0 || strings.Contains(err.Error(), "private-probe-input") {
			t.Fatal("unknown micro variant did not fail closed without retaining input")
		}
	}
}

func TestStructuredLayerProbeMicroRequestIdentity(t *testing.T) {
	model := llm.Model{ID: "synthetic-opus", ReasoningEffort: llm.ReasoningEffortHigh}
	env := []string{"HOME=/synthetic-home", "PATH=/usr/bin"}
	for _, offline := range probeMicroSchemaVariants(t) {
		t.Run(offline.Name, func(t *testing.T) {
			selected := selectedProbeSchemaVariant(t, offline.Name)
			invocation := newStructuredProbeInvocation(model, offline.Schema)
			baseline, err := buildStructuredRequest(model, env, offline.Schema, "/private-temp/system.txt", invocation.Input, false)
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			var generationContext context.Context
			report := runStructuredProbeVariant(t.Context(), selected, model, func(ctx context.Context, got structuredProbeInvocation, variant probeSchemaVariant) structuredProbeDiagnostic {
				count++
				generationContext = ctx
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > invocation.Timeout || got.Timeout != invocation.Timeout {
					t.Fatal("micro timeout changed")
				}
				request, err := buildStructuredRequest(got.Model, env, variant.Schema, "/private-temp/system.txt", got.Input, false)
				if err != nil || !reflect.DeepEqual(request, baseline) || got.Model != invocation.Model || got.System != invocation.System || got.Input != invocation.Input ||
					!bytes.Equal(variant.Schema.document, offline.Schema.document) {
					t.Fatal("selected micro variant changed the shared offline request/schema/prompt/settings")
				}
				var initialize, payload map[string]jsontext.Value
				if json.Unmarshal(request.Initialize, &initialize) != nil || json.Unmarshal(initialize["request"], &payload) != nil ||
					!bytes.Equal(payload["jsonSchema"], offline.Schema.document) || string(payload["sdkMcpServers"]) != "[]" || string(payload["hooks"]) != "{}" {
					t.Fatal("wire initialize schema bytes/hash differ from the offline builder")
				}
				return runStructuredProbeVariantFixture(t, variant, structuredProbeStream(structuredProbeFinal))
			})
			if count != 1 || report.Level != 0 || report.Variant != offline.Name || !report.Diagnostic.accepted() || generationContext.Err() != context.Canceled ||
				report.SchemaBytes != len(offline.Schema.document) || report.SchemaSHA256 != fmt.Sprintf("%x", sha256.Sum256(offline.Schema.document)) {
				t.Fatal("micro selected another level, lost schema identity or retained a generation context")
			}
		})
	}
}

func TestStructuredLayerProbeMicroOutcomesAndSecurity(t *testing.T) {
	for _, variant := range probeMicroSchemaVariants(t) {
		t.Run(variant.Name, func(t *testing.T) {
			d := runStructuredProbeVariantFixture(t, variant, structuredProbeStream(structuredProbeFinal))
			if !d.accepted() || d.Outcome != probeValidatedFinal || !d.StructuredOutputPresent || !d.SchemaChecked || !d.SchemaValid {
				t.Fatal("micro variant did not validate its Final")
			}
			d = runStructuredProbeVariantFixture(t, variant, structuredProbeStream(structuredProbeC2Witness))
			if variant.Name == "C2" {
				if !d.accepted() || d.Outcome != probeValidatedAction || !d.SchemaValid {
					t.Fatal("minimum C2 union witness was not classified without execution")
				}
				// A type-only union witness cannot become a production proposal.
				if _, err := structuredProbeSchema(t, 2).parse(jsontext.Value(structuredProbeC2Witness)); err == nil {
					t.Fatal("type-only witness became a production Action")
				}
			} else if d.accepted() || d.Outcome != probeSchemaInvalid {
				t.Fatal("single-branch C1 accepted an Action")
			}
			d = runStructuredProbeVariantFixture(t, variant, structuredProbeControl+diagnosticInit+string(layerCorpus(t, "probe-provider-unknown")))
			if d.Outcome != probeProviderFailure || d.Stage != structuredResultInvalid || d.AssistantError != assistantUnknown ||
				d.StructuredOutputPresent || d.SchemaChecked || d.SchemaValid || d.Details == nil ||
				d.Details.UnexpectedMetadataScope != "assistant" || d.Details.UnexpectedMetadataField != "is_api_error_message" ||
				d.Details.StructuredOutputBlocks != 0 {
				t.Fatal("observed C provider error was reclassified or leaked into schema validation", d.summary())
			}
			for _, tool := range []string{"Bash", "Read", "Write", "Edit", "Glob", "Grep", "Agent", "Web", "mcp__foreign__execute", "UnknownTool"} {
				stream := structuredProbeControl + diagnosticInit + `{"type":"assistant","message":{"model":"synthetic-model","content":[{"type":"tool_use","id":"private-probe-id","name":"` + tool + `","input":{"command":"private-probe-command"}}]}}` + "\n"
				d := runStructuredProbeVariantFixture(t, variant, stream)
				if d.accepted() || d.Outcome != probeProtocolFailure || d.Stage != structuredExecutionRejected {
					t.Fatal("micro unsafe-tool rejection changed", d.summary())
				}
			}
			d = runStructuredProbeVariantFixture(t, variant, structuredProbeStream(structuredProbeFinal)+`{"type":"tool_progress","tool_name":"Bash","tool_use_id":"private-probe-tail-id"}`+"\n")
			if d.accepted() || d.Outcome != probeProtocolFailure || !d.SchemaChecked || !d.SchemaValid {
				t.Fatal("earlier micro schema success masked trailing execution")
			}
		})
	}
}

func TestStructuredOfficialControlProbeMicroOutcomesAndIsolation(t *testing.T) {
	c, f, _, _ := structuredClient(t)
	f.Set(t, testclaude.Config{Subscription: "team", BridgeManagedPermissionsOnly: true, StructuredResponses: []string{structuredProbeFinal}})
	path, env, err := c.prepare(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	model := llm.Model{ID: "synthetic-opus", ReasoningEffort: llm.ReasoningEffortHigh}
	for _, variant := range probeMicroSchemaVariants(t) {
		t.Run(variant.Name, func(t *testing.T) {
			f.Set(t, testclaude.Config{Subscription: "team", BridgeManagedPermissionsOnly: true, StructuredResponses: []string{structuredProbeFinal}})
			invocation := newStructuredProbeInvocation(model, variant.Schema)
			ctx, cancel := context.WithTimeout(t.Context(), invocation.Timeout)
			_, response, err := c.structuredGeneration(ctx, path, env, model, variant.Schema, invocation.System, invocation.Input, false)
			cancel()
			if err != nil || len(response.Output) != 0 {
				t.Fatal("micro existing offline adapter baseline failed")
			}
			calls := f.Calls(t)
			offline := normalizedProbeCall(t, calls[len(calls)-1])
			cases := []struct {
				name    string
				config  testclaude.Config
				outcome probeOutcome
				accept  bool
			}{
				{"final", testclaude.Config{StructuredResponses: []string{structuredProbeFinal}}, probeValidatedFinal, true},
				{"provider", testclaude.Config{BridgeEvent: string(layerCorpus(t, "probe-provider-unknown"))}, probeProviderFailure, false},
				{"protocol", testclaude.Config{BridgeEvent: `{"type":"assistant","message":{"model":"synthetic-model","content":[{"type":"tool_use","id":"private-probe-id","name":"Bash","input":{"command":"private-probe-command"}}]}}`}, probeProtocolFailure, false},
				{"fresh-after-failures", testclaude.Config{StructuredResponses: []string{structuredProbeFinal}}, probeValidatedFinal, true},
			}
			if variant.Name == "C2" {
				cases = append(cases, struct {
					name    string
					config  testclaude.Config
					outcome probeOutcome
					accept  bool
				}{
					"inert-action", testclaude.Config{StructuredResponses: []string{structuredProbeC2Witness}}, probeValidatedAction, true,
				})
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					tc.config.Subscription, tc.config.BridgeManagedPermissionsOnly = "team", true
					f.Set(t, tc.config)
					before := len(f.Calls(t))
					report := runStructuredProbeVariant(t.Context(), variant, model, func(ctx context.Context, invocation structuredProbeInvocation, variant probeSchemaVariant) structuredProbeDiagnostic {
						return structuredProbeVariantGeneration(ctx, path, env, invocation, variant)
					})
					if report.Diagnostic.Outcome != tc.outcome || report.Diagnostic.accepted() != tc.accept || report.Variant != variant.Name || report.Level != 0 ||
						report.SchemaBytes != len(variant.Schema.document) || report.SchemaSHA256 != fmt.Sprintf("%x", sha256.Sum256(variant.Schema.document)) {
						t.Fatal("micro fake outcome/schema differs", report.Diagnostic.summary())
					}
					assertProbeVariantValueFree(t, report.Diagnostic)
					calls := f.Calls(t)
					if len(calls) != before+1 || len(f.CatalogCalls(t)) != 0 || len(f.StructuredProbes(t)) != 0 {
						t.Fatal("micro probe invoked another generation, registry, MCP or catalog")
					}
					call := calls[len(calls)-1]
					if !reflect.DeepEqual(offline, normalizedProbeCall(t, call)) {
						t.Fatal("micro CLI differs from the existing offline request path")
					}
					want, err := buildStructuredRequest(model, env, variant.Schema, probeSystemPath(t, call), structuredProbeInput, false)
					if err != nil || call.Initialize != string(want.Initialize) || call.UserFrame != string(want.Input) || !slices.Equal(call.Arguments, want.Arguments) ||
						!slices.Equal(call.Environment, want.Environment) || call.System != structuredProbeSystem || call.Input != structuredProbeInput {
						t.Fatal("micro fake wire data differs from the shared builder")
					}
					argument(t, call.Arguments, "--tools", "")
					argument(t, call.Arguments, "--allowedTools", "StructuredOutput")
					argument(t, call.Arguments, "--disallowedTools", structuredDeny)
					argument(t, call.Arguments, "--mcp-config", `{"mcpServers":{}}`)
					for _, option := range []string{"--resume", "--continue", "--session-id", "--fork-session", "--fallback-model"} {
						if slices.Contains(call.Arguments, option) {
							t.Fatal("micro probe has continuation or fallback settings")
						}
					}
					t.Logf("variant=%s %s execution=0 MCP=0 Operations=0", variant.Name, report.Diagnostic.summary())
				})
			}
		})
	}
	seen, directories := map[int]bool{}, map[string]bool{}
	for _, call := range f.Calls(t) {
		if call.PID <= 0 || seen[call.PID] || directories[call.Directory] || call.ChildPID != 0 || !errors.Is(syscall.Kill(call.PID, 0), syscall.ESRCH) {
			t.Fatal("micro probe reused process/parser storage, spawned a child or failed to reap")
		}
		seen[call.PID], directories[call.Directory] = true, true
		if _, err := os.Stat(call.Directory); !os.IsNotExist(err) {
			t.Fatal("micro probe left a private prompt or temporary directory")
		}
	}
}

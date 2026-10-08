//go:build linux || darwin

package claudecode

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

const structuredProbeVariantBHash = "a640f9b702ffed27b4be45e97e8f357c5d906d8809f8f7a4d9569da6b4382e82"
const structuredProbeVariantBWitness = `{"type":"final","final":{"message":"adapter schema probe OK"},"action":{"id":"private-probe-inert-witness"}}`

func TestStructuredLayerProbeVariantSelection(t *testing.T) {
	for _, tc := range []struct {
		ladder, exact int
		variant       string
		invalid       bool
	}{
		{0, 0, "", false}, {1, 0, "", false}, {5, 0, "", false}, {0, 2, "", false},
		{0, 0, "A", false}, {0, 0, "B", false}, {0, 0, "C", false},
		{0, 0, "D", false}, {0, 0, "E", false}, {0, 0, "F", false},
		{1, 0, "B", true}, {5, 0, "B", true}, {0, 1, "B", true}, {0, 5, "B", true},
		{1, 1, "B", true}, {-1, 0, "B", true}, {0, 6, "B", true},
		{0, 0, "b", true}, {0, 0, "AB", true}, {0, 0, "G", true},
		{0, 0, " B", true}, {0, 0, "private-probe-selector", true},
	} {
		t.Run(fmt.Sprintf("%d-%d-%s", tc.ladder, tc.exact, tc.variant), func(t *testing.T) {
			selection, err := selectStructuredProbe(tc.ladder, tc.exact, tc.variant)
			if (err != nil) != tc.invalid {
				t.Fatal("variant conflict/range was not rejected")
			}
			if err != nil {
				if strings.Contains(err.Error(), "private-probe-") || len(selection.Levels) != 0 || selection.Variant != "" {
					t.Fatal("invalid selection retained input or selected a request")
				}
				return
			}
			if tc.variant != "" {
				if len(selection.Levels) != 0 || selection.Variant != tc.variant {
					t.Fatal("variant visits a level or another schema")
				}
			} else {
				levels, _ := selectStructuredProbeLevels(tc.ladder, tc.exact)
				if !slices.Equal(selection.Levels, levels) || selection.Variant != "" {
					t.Fatal("existing level selection changed")
				}
			}
		})
	}
	registered := flag.Lookup("claude-structured-probe-variant")
	if registered == nil || registered.DefValue != "" {
		t.Fatal("variant real inference is not opt-in")
	}
	for _, args := range [][]string{
		{"-claude-structured-probe-variant=B"},
		{"-claude-structured-probe-variant=B", "-claude-structured-probe-levels=0"},
		{"-claude-structured-probe-exact-level=0", "-claude-structured-probe-variant=B"},
	} {
		flags := flag.NewFlagSet("offline-selection", flag.ContinueOnError)
		ladder := flags.Int("claude-structured-probe-levels", 0, "")
		exact := flags.Int("claude-structured-probe-exact-level", 0, "")
		variant := flags.String("claude-structured-probe-variant", "", "")
		if flags.Parse(args) != nil {
			t.Fatal("synthetic option parse failed")
		}
		selection, err := structuredProbeSelectionFromFlags(flags, *ladder, *exact, *variant)
		if (err != nil) != (len(args) > 1) || err == nil && selection.Variant != "B" {
			t.Fatal("explicit zero level flag bypassed variant exclusivity")
		}
	}
}

func runStructuredProbeVariantFixture(t *testing.T, variant probeSchemaVariant, stream string) structuredProbeDiagnostic {
	t.Helper()
	var stdin bytes.Buffer
	reader := newStructuredProbeVariantReader(strings.NewReader(stream), variant)
	p, response, err := runStructuredProtocol(reader, &stdin, variant.Schema, structuredProbeInput, false)
	if p.Action != nil || len(response.Output) != 0 {
		t.Fatal("schema-only variant became an Action or public ToolCall")
	}
	d := reader.diagnostic(response, err)
	assertProbeVariantValueFree(t, d)
	return d
}

func assertProbeVariantValueFree(t *testing.T, d structuredProbeDiagnostic) {
	t.Helper()
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal("diagnostic encoding failed")
	}
	for _, value := range []string{"private-probe-", "private-helper-", "serializer-only", "fixture.go", "account-sensitive", "synthetic-opus"} {
		if bytes.Contains(raw, []byte(value)) || strings.Contains(d.summary(), value) {
			t.Fatal("variant diagnostic retained a body, ID, arguments or private value")
		}
	}
}

func TestStructuredLayerProbeVariantOutcomeMatrix(t *testing.T) {
	variant := selectedProbeSchemaVariant(t, "B")
	for _, tc := range []struct {
		name, stream string
		outcome      probeOutcome
		stage        structuredStage
		accepted     bool
	}{
		{"final", structuredProbeStream(structuredProbeFinal), probeValidatedFinal, 0, true},
		{"inert-optional-action", structuredProbeStream(structuredProbeVariantBWitness), probeValidatedAction, 0, true},
		{"missing", structuredProbeStream(""), probeStructuredMissing, structuredResultMissingOutput, false},
		{"null", structuredProbeStream("null"), probeStructuredNull, structuredResultNullOutput, false},
		{"schema", structuredProbeStream(`{"type":"action","action":{}}`), probeSchemaInvalid, structuredSchemaRejected, false},
		{"envelope", structuredProbeStream(`{"type":"final","final":{"message":"adapter schema probe OK"},"action":{"id":5}}`), probeSchemaInvalid, structuredSchemaRejected, false},
		{"provider", structuredProbeControl + diagnosticInit + string(layerCorpus(t, "probe-provider-unknown")), probeProviderFailure, structuredResultInvalid, false},
		{"protocol", structuredProbeControl + diagnosticInit + `{"type":"control_request","request_id":"private-probe-id","request":{"subtype":"can_use_tool"}}` + "\n", probeProviderFailure, structuredUnexpectedControlFrame, false},
		{"incomplete", structuredProbeControl + diagnosticInit + structuredProbeSerializer, probeProtocolFailure, structuredStreamIncomplete, false},
		{"trailing-execution", structuredProbeStream(structuredProbeVariantBWitness) + `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","id":"private-probe-id","input":{"command":"private-probe-command"}}]}}` + "\n", probeProtocolFailure, structuredTrailingFrame, false},
		{"duplicate-json", structuredProbeStream(`{"type":"final","type":"final","final":{"message":"adapter schema probe OK"}}`), probeProtocolFailure, structuredDuplicateFields, false},
		{"free-text", structuredProbeStream(`"private-probe-run-command"`), probeSchemaInvalid, structuredSchemaRejected, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := runStructuredProbeVariantFixture(t, variant, tc.stream)
			if d.Outcome != tc.outcome || d.Stage != tc.stage || d.accepted() != tc.accepted {
				t.Fatal("variant outcome changed", d.summary())
			}
			if tc.accepted && (!d.SchemaChecked || !d.SchemaValid || !d.StructuredOutputPresent || d.StructuredOutputNull) {
				t.Fatal("accepted variant bypassed original schema validation")
			}
			if tc.name == "provider" && (d.AssistantError != assistantUnknown || d.SchemaChecked || d.StructuredOutputPresent || d.Details.UnexpectedMetadataField != "is_api_error_message") {
				t.Fatal("provider reason/metadata field was lost or reached schema validation")
			}
		})
	}
	// The optional witness is a probe-only outcome. Production envelope parsing
	// remains exclusive, and a type=action value still fails the frozen B schema.
	if _, err := variant.Schema.parse(jsontext.Value(structuredProbeVariantBWitness)); err == nil {
		t.Fatal("production Action/Final semantics changed")
	}
}

func TestStructuredLayerProbeVariantSchemaOnlyValidation(t *testing.T) {
	for _, variant := range probeSchemaVariants(t) {
		t.Run(variant.Name, func(t *testing.T) {
			for _, raw := range []string{structuredProbeFinal, probeVariantWitness(t, variant.Name)} {
				d := runStructuredProbeVariantFixture(t, variant, structuredProbeStream(raw))
				if !d.accepted() {
					t.Fatal("schema-valid inert witness failed", d.summary())
				}
			}
		})
	}
	variant := selectedProbeSchemaVariant(t, "B")
	for _, invalid := range []string{
		`{"type":"final","final":{"message":"adapter schema probe OK"},"action":{"id":false}}`,
		`{"type":"final","final":{"message":"adapter schema probe OK"},"action":{"tool":"Bash"}}`,
		`{"type":"final","final":{"message":"adapter schema probe OK"},"extra":"private-probe-value"}`,
		`{"type":"final","final":{"message":"private-probe-unexpected-prose"},"action":{}}`,
		`{"type":"final","final":{"message":"adapter schema probe OK"},"action":null}`,
		`{"type":"final","final":{"message":"` + strings.Repeat("x", structuredResponseBytes) + `"}}`,
	} {
		d := validateStructuredProbeVariantOutput(variant, jsontext.Value(invalid))
		if d.accepted() {
			t.Fatal("invalid/wrong-message/oversized variant was accepted")
		}
		assertProbeVariantValueFree(t, d)
	}
	for _, tool := range []string{"Bash", "Read", "Write", "Edit", "Glob", "Grep", "Agent", "Web", "mcp__foreign__read", "unknown"} {
		frame, _ := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"model": "synthetic-model", "content": []any{map[string]any{"type": "tool_use", "id": "private-probe-tool-id", "name": tool, "input": map[string]any{}}}}})
		d := runStructuredProbeVariantFixture(t, variant, structuredProbeControl+diagnosticInit+string(frame)+"\n"+structuredProbeStream(structuredProbeVariantBWitness))
		if d.accepted() || d.Stage != structuredExecutionRejected {
			t.Fatal("variant projection hid side-effecting/foreign tool execution")
		}
	}
	for _, field := range []string{"deferred_tool_use", "tool_use_result"} {
		stream := strings.Replace(structuredProbeStream(structuredProbeVariantBWitness), `"type":"result","subtype":"success"`, `"type":"result","subtype":"success","`+field+`":{"tool":"Bash","input":"private-probe-command"}`, 1)
		d := runStructuredProbeVariantFixture(t, variant, stream)
		if d.accepted() || d.Stage != structuredExecutionRejected {
			t.Fatal("variant projection removed execution metadata from the result")
		}
	}
	// A malformed/oversized authoritative value must not be made valid by the
	// inert stream projection, nor may a duplicate/trailing JSON value be hidden.
	for _, raw := range []string{`{`, structuredProbeFinal + ` {}`, `{"type":"final","type":"final","final":{"message":"adapter schema probe OK"}}`} {
		if d := validateStructuredProbeVariantOutput(variant, jsontext.Value(raw)); d.accepted() || d.SchemaValid {
			t.Fatal("variant schema-only parser accepted malformed/duplicate/trailing JSON")
		}
	}
	oversized := structuredProbeControl + diagnosticInit + `{"type":"result","subtype":"success","structured_output":{"type":"final","final":{"message":"` + strings.Repeat("x", 2<<20) + `"}}}` + "\n"
	if d := runStructuredProbeVariantFixture(t, variant, oversized); d.accepted() || d.Stage != structuredResponseOversized {
		t.Fatal("variant projection concealed the original wire frame size")
	}
}

func probeVariantWitness(t *testing.T, name string) string {
	t.Helper()
	switch name {
	case "A":
		return structuredProbeFinal
	case "B":
		return structuredProbeVariantBWitness
	case "C":
		return `{"type":"action","action":{"id":"private-probe-inert-witness"}}`
	case "D":
		return `{"type":"action","action":{"id":"private-probe-inert-witness","tool":"probe_proposal_only"}}`
	case "E":
		return `{"type":"action","action":{"id":"private-probe-inert-witness","tool":"probe_proposal_only","arguments":{}}}`
	default:
		return structuredProbeWitness(t, 2)
	}
}

func TestStructuredLayerProbeVariantControllerRequestIdentity(t *testing.T) {
	model := llm.Model{ID: "synthetic-opus", ReasoningEffort: llm.ReasoningEffortHigh}
	for _, offline := range probeSchemaVariants(t) {
		t.Run(offline.Name, func(t *testing.T) {
			selection, err := selectStructuredProbe(0, 0, offline.Name)
			if err != nil {
				t.Fatal(err)
			}
			selected := selectedProbeSchemaVariant(t, selection.Variant)
			invocation := newStructuredProbeInvocation(model, offline.Schema)
			baseline, err := buildStructuredRequest(model, []string{"HOME=/synthetic-home", "PATH=/usr/bin"}, offline.Schema, "/private-temp/system.txt", invocation.Input, false)
			if err != nil {
				t.Fatal(err)
			}
			var contextUsed context.Context
			count := 0
			report := runStructuredProbeVariant(t.Context(), selected, model, func(ctx context.Context, got structuredProbeInvocation, variant probeSchemaVariant) structuredProbeDiagnostic {
				count++
				contextUsed = ctx
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > invocation.Timeout || got.Timeout != invocation.Timeout {
					t.Fatal("variant timeout changed")
				}
				request, err := buildStructuredRequest(got.Model, []string{"HOME=/synthetic-home", "PATH=/usr/bin"}, variant.Schema, "/private-temp/system.txt", got.Input, false)
				if err != nil || !reflect.DeepEqual(request, baseline) || got.Model != invocation.Model || got.System != invocation.System || got.Input != invocation.Input || !bytes.Equal(variant.Schema.document, offline.Schema.document) {
					t.Fatal("variant selector changed request/schema/prompt/system/timeout/session settings")
				}
				return runStructuredProbeVariantFixture(t, variant, structuredProbeStream(probeVariantWitness(t, variant.Name)))
			})
			if count != 1 || report.Variant != offline.Name || report.Level != 0 || !report.Diagnostic.accepted() || contextUsed.Err() != context.Canceled {
				t.Fatal("variant executed extra generations, retained a deadline or selected a level")
			}
			if offline.Name == "B" && (report.SchemaBytes != 329 || report.SchemaSHA256 != structuredProbeVariantBHash) {
				t.Fatal("B wire bytes/hash differ from offline audit")
			}
		})
	}
}

func TestStructuredOfficialControlProbeVariantBOutcomesAndIsolation(t *testing.T) {
	c, f, _, _ := structuredClient(t)
	f.Set(t, testclaude.Config{Subscription: "team", BridgeManagedPermissionsOnly: true, StructuredResponses: []string{structuredProbeFinal}})
	path, env, err := c.prepare(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	variant := selectedProbeSchemaVariant(t, "B")
	model := llm.Model{ID: "synthetic-opus", ReasoningEffort: llm.ReasoningEffortHigh}
	// Compare actual received traffic to the pre-existing offline adapter path,
	// not only to another invocation of the new variant harness.
	baseline := newStructuredProbeInvocation(model, variant.Schema)
	ctx, cancel := context.WithTimeout(t.Context(), baseline.Timeout)
	_, response, err := c.structuredGeneration(ctx, path, env, model, variant.Schema, baseline.System, baseline.Input, false)
	cancel()
	if err != nil || len(response.Output) != 0 {
		t.Fatal("existing offline adapter baseline failed")
	}
	offline := normalizedProbeCall(t, f.Calls(t)[0])
	for _, tc := range []struct {
		name    string
		config  testclaude.Config
		outcome probeOutcome
		accept  bool
	}{
		{"final", testclaude.Config{StructuredResponses: []string{structuredProbeFinal}}, probeValidatedFinal, true},
		{"action", testclaude.Config{StructuredResponses: []string{structuredProbeVariantBWitness}}, probeValidatedAction, true},
		{"provider", testclaude.Config{BridgeEvent: string(layerCorpus(t, "probe-provider-unknown"))}, probeProviderFailure, false},
		{"protocol", testclaude.Config{BridgeEvent: `{"type":"assistant","message":{"model":"synthetic-model","content":[{"type":"tool_use","id":"private-probe-id","name":"Bash","input":{"command":"private-probe-command"}}]}}`}, probeProtocolFailure, false},
		{"fresh-after-failures", testclaude.Config{StructuredResponses: []string{structuredProbeFinal}}, probeValidatedFinal, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.config.Subscription, tc.config.BridgeManagedPermissionsOnly = "team", true
			f.Set(t, tc.config)
			before := len(f.Calls(t))
			report := runStructuredProbeVariant(t.Context(), variant, model, func(ctx context.Context, invocation structuredProbeInvocation, variant probeSchemaVariant) structuredProbeDiagnostic {
				return structuredProbeVariantGeneration(ctx, path, env, invocation, variant)
			})
			if report.Diagnostic.Outcome != tc.outcome || report.Diagnostic.accepted() != tc.accept || report.SchemaBytes != 329 || report.SchemaSHA256 != structuredProbeVariantBHash || report.Variant != "B" || report.Level != 0 {
				t.Fatal("fake variant outcome/schema differs", report.Diagnostic.summary())
			}
			assertProbeVariantValueFree(t, report.Diagnostic)
			if tc.name == "protocol" && report.Diagnostic.Stage != structuredExecutionRejected {
				t.Fatal("fake protocol failure did not reach the side-effecting tool rejection")
			}
			calls := f.Calls(t)
			if len(calls) != before+1 || len(f.CatalogCalls(t)) != 0 || len(f.StructuredProbes(t)) != 0 {
				t.Fatal("variant invoked another level, retry, MCP or catalog")
			}
			call := calls[len(calls)-1]
			if !reflect.DeepEqual(offline, normalizedProbeCall(t, call)) {
				t.Fatal("variant CLI request differs from the existing offline audit request")
			}
			want, err := buildStructuredRequest(model, env, variant.Schema, filepath.Join(call.Directory, "system.txt"), structuredProbeInput, false)
			if err != nil || call.Initialize != string(want.Initialize) || call.UserFrame != string(want.Input) || !slices.Equal(call.Arguments, want.Arguments) || !slices.Equal(call.Environment, want.Environment) || call.System != structuredProbeSystem || call.Input != structuredProbeInput {
				t.Fatal("variant adapter duplicated/changed the existing request builder")
			}
			argument(t, call.Arguments, "--tools", "")
			argument(t, call.Arguments, "--allowedTools", "StructuredOutput")
			argument(t, call.Arguments, "--disallowedTools", structuredDeny)
			argument(t, call.Arguments, "--mcp-config", `{"mcpServers":{}}`)
			for _, flag := range []string{"--resume", "--continue", "--session-id", "--fork-session", "--fallback-model"} {
				if slices.Contains(call.Arguments, flag) {
					t.Fatal("variant has continuation or fallback options")
				}
			}
			t.Logf("variant=B %s execution=0 MCP=0 Operations=0", report.Diagnostic.summary())
		})
	}
	seen := map[int]bool{}
	for _, call := range f.Calls(t) {
		if call.PID <= 0 || seen[call.PID] || call.ChildPID != 0 || !errors.Is(syscall.Kill(call.PID, 0), syscall.ESRCH) {
			t.Fatal("variant reused or failed to reap its subprocess")
		}
		seen[call.PID] = true
		if _, err := os.Stat(call.Directory); !os.IsNotExist(err) {
			t.Fatal("variant left a private prompt/temporary directory")
		}
	}
}

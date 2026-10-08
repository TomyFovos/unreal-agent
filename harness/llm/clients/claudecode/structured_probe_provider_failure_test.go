//go:build linux || darwin

package claudecode

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"slices"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

// The error envelope/text-only shape is observed. The boolean spelling in this
// synthetic witness comes from the installed CLI serializer, not a captured
// provider payload: the operator's actual unknown boolean is still unconfirmed.
func TestStructuredLayerProbeObservedUnknownProviderFailure(t *testing.T) {
	schema := structuredProbeSchema(t, 2)
	frame := layerCorpus(t, "probe-provider-unknown")
	var stdin bytes.Buffer
	p, response, err := runStructuredProtocol(bytes.NewReader(append([]byte(structuredProbeControl+diagnosticInit), frame...)), &stdin, schema, structuredProbeInput, false)
	d := classifyStructuredProbeLevel(2, p, response, err)
	if d.Outcome != probeProviderFailure || d.Reason != probeTypedFailure || d.Stage != structuredResultInvalid ||
		d.AssistantError != assistantUnknown || d.accepted() || p.Action != nil || p.Final != nil || len(response.Output) != 0 {
		t.Fatal("upstream unknown error became a schema error, proposal or public response")
	}
	if d.Details == nil || d.Details.AssistantReason != "assistant_unknown" || d.Details.StructuredOutputBlocks != 0 ||
		d.Details.SchemaChecked || d.Details.SchemaValid || d.Details.StructuredOutputPresent ||
		!slices.Equal(d.Details.ContentBlockTypes, []string{"text"}) || len(d.Details.ToolNameKinds) != 0 {
		t.Fatal("provider error incorrectly reached serializer/result validation")
	}
	if d.Details.UnexpectedMetadataScope != "assistant" || d.Details.UnexpectedMetadataField != "is_api_error_message" {
		t.Fatal("provider failure hides the diagnostic-only unknown boolean name")
	}
	assertProbeFailureValueFree(t, d, err)
}

func assertProbeFailureValueFree(t *testing.T, d structuredProbeDiagnostic, err error) {
	t.Helper()
	encoded, marshalErr := json.Marshal(d)
	if marshalErr != nil {
		t.Fatal("safe diagnostic encoding failed")
	}
	for _, secret := range []string{"private-probe-error-body", "private-probe-message-id", "private-probe-request-id", "private-probe-session-id", "private-probe-uuid", "synthetic-opus", "2000-01-01T00:00:00Z"} {
		if bytes.Contains(encoded, []byte(secret)) || strings.Contains(d.summary(), secret) || strings.Contains(err.Error(), secret) {
			t.Fatal("provider error retained a private body, value or identity")
		}
	}
}

func TestStructuredLayerProbeProviderFailureMetadataNames(t *testing.T) {
	for _, tc := range []struct{ name, report string }{
		{"future_boolean", "future_boolean"},
		{"Future.metadata-9_0", "Future.metadata-9_0"},
		{strings.Repeat("f", 64), strings.Repeat("f", 64)},
		{strings.Repeat("f", 65), "unknown"},
		{"future_メタデータ", "unknown"},
		{"future:metadata", "unknown"},
		{"future\nmetadata", "unknown"},
		{"", "unknown"},
	} {
		t.Run(tc.report, func(t *testing.T) {
			var fields map[string]jsontext.Value
			if json.Unmarshal(layerCorpus(t, "probe-provider-unknown"), &fields) != nil {
				t.Fatal("synthetic fixture malformed")
			}
			delete(fields, "is_api_error_message")
			fields[tc.name] = jsontext.Value(`true`)
			frame, _ := json.Marshal(fields, json.Deterministic(true))
			var stdin bytes.Buffer
			p, response, err := runStructuredProtocol(strings.NewReader(structuredProbeControl+diagnosticInit+string(frame)+"\n"), &stdin, structuredProbeSchema(t, 2), structuredProbeInput, false)
			d := classifyStructuredProbeLevel(2, p, response, err)
			// A name that sorts before error can be the parser's rejection; a
			// later name is only observed while projecting a provider failure.
			if d.Details == nil || d.Details.UnexpectedMetadataScope != "assistant" || d.Details.UnexpectedMetadataField != tc.report ||
				p.Action != nil || p.Final != nil || len(response.Output) != 0 || d.Details.SchemaChecked || d.accepted() {
				t.Fatal("unknown assistant key lost its bounded name or reached execution")
			}
			assertProbeFailureValueFree(t, d, err)
			encoded, _ := json.Marshal(d)
			if tc.report == "unknown" && tc.name != "" {
				key, _ := json.Marshal(tc.name)
				if bytes.Contains(encoded, key) || strings.Contains(d.summary(), tc.name) {
					t.Fatal("unsafe metadata member name leaked")
				}
			}
		})
	}
}

func TestStructuredLayerProbeAPIMetadataIsNotNewAcceptance(t *testing.T) {
	frame := `{"type":"assistant","is_api_error_message":true,"message":{"model":"synthetic-opus","content":[{"type":"tool_use","id":"private-probe-message-id","name":"StructuredOutput","input":{}}]}}`
	var stdin bytes.Buffer
	p, response, err := runStructuredProtocol(strings.NewReader(structuredProbeControl+diagnosticInit+frame+"\n"+diagnosticFinal), &stdin, structuredProbeSchema(t, 2), structuredProbeInput, false)
	e := requireStructuredStage(t, err, "structured_serializer_frame_invalid")
	if e.StructuredDetails().AssistantReason != "assistant_unexpected_metadata" || e.StructuredDetails().UnexpectedMetadataField != "is_api_error_message" ||
		p.Action != nil || p.Final != nil || len(response.Output) != 0 || e.StructuredDetails().SchemaChecked {
		t.Fatal("diagnostic field-name exposure became metadata acceptance")
	}
}

func TestStructuredOfficialControlProbeUnknownErrorNeverExecutes(t *testing.T) {
	c, f, _, _ := structuredClient(t)
	f.Set(t, testclaude.Config{Subscription: "team", BridgeEvent: string(layerCorpus(t, "probe-provider-unknown"))})
	path, env, err := c.prepare(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var output []llm.Item
	reports := runStructuredProbeSequence(t.Context(), []int{2}, llm.Model{ID: "synthetic-opus", ReasoningEffort: llm.ReasoningEffortHigh},
		func(level int) *actionSchema { return structuredProbeSchema(t, level) },
		func(ctx context.Context, model llm.Model, schema *actionSchema, system, input string) (actionProposal, llm.Response, error) {
			p, response, err := c.structuredGeneration(ctx, path, env, model, schema, system, input, false)
			output = response.Output
			return p, response, err
		})
	if len(reports) != 1 || reports[0].Diagnostic.Outcome != probeProviderFailure || reports[0].Diagnostic.AssistantError != assistantUnknown ||
		len(output) != 0 || len(f.Calls(t)) != 1 || len(f.CatalogCalls(t)) != 0 || len(f.StructuredProbes(t)) != 0 {
		t.Fatal("provider failure retried, leaked public output or left the isolated probe")
	}
	// This adapter-only fixture has no action callback, Host, Session or executor;
	// there is no path that could create an Operation or execute free text.
	t.Log("observed text-only assistant_unknown: provider_failure; schema_checked=false; execution/MCP/Operations/public responses=0")
}

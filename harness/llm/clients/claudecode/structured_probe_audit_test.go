//go:build linux || darwin

package claudecode

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/unreallabsai/unreal-agent/harness/llm"
)

const structuredProbeSchemaSHA256 = "c703d71094a2f7c5555ae6d194a12293917421e44e6f4e930ffd86e9b1d9cfd9"
const structuredProbeFinal = `{"type":"final","final":{"message":"adapter schema probe OK"}}`
const structuredProbeControl = `{"type":"control_response","response":{"subtype":"success","request_id":"unreal_structured_init","response":{}}}` + "\n"

// The same SDK shapes, helper IDs and partial positions deliberately recur in
// every level. Any parser or serializer state leaking across requests breaks
// this prefix test, without a real CLI, account, network or work executor.
const structuredProbeSerializer = `{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"serializer","name":"StructuredOutput","input":{}}}}` + "\n" +
	`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{}"}}}` + "\n" +
	`{"type":"stream_event","event":{"type":"content_block_stop","index":0}}` + "\n" +
	`{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"tool_use"}}}` + "\n" +
	`{"type":"stream_event","event":{"type":"message_stop"}}` + "\n" +
	`{"type":"assistant","message":{"model":"synthetic-model","content":[{"type":"tool_use","id":"serializer","name":"StructuredOutput","input":{"internal":"private-probe-helper"}}],"stop_reason":"tool_use"}}` + "\n" +
	`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"serializer","content":"private-probe-helper"}]}}` + "\n"

func structuredProbeStream(output string) string {
	field := ""
	if output != "" {
		field = `,"structured_output":` + output
	}
	return structuredProbeControl + diagnosticInit + structuredProbeSerializer +
		`{"type":"result","subtype":"success","is_error":false` + field + `}` + "\n"
}

type probeRequestSnapshot struct {
	Model         llm.Model
	System, Input string
	Timeout       time.Duration
	Request       structuredRequest
}

func TestStructuredLayerProbeRequestIndependence(t *testing.T) {
	c := &Client{config: Config{ManagedPolicyMode: ManagedPolicyTrust, Getenv: func(key string) string {
		if key == "HOME" {
			return "/synthetic-home"
		}
		return ""
	}}}
	env, err := c.environment()
	if err != nil {
		t.Fatal(err)
	}
	model := llm.Model{ID: "synthetic-opus", ReasoningEffort: llm.ReasoningEffortHigh}
	capture := func(levels int) ([]probeRequestSnapshot, []structuredProbeReport) {
		var requests []probeRequestSnapshot
		var contexts []context.Context
		reports := runStructuredProbeLevels(t.Context(), levels, model,
			func(level int) *actionSchema { return structuredProbeSchema(t, level) },
			func(ctx context.Context, got llm.Model, schema *actionSchema, system, input string) (actionProposal, llm.Response, error) {
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > structuredProbeTimeout {
					t.Fatal("generation deadline missing or different")
				}
				contexts = append(contexts, ctx)
				// Normalize only the private temporary directory name. Its contents
				// (System), all other argv values and every wire byte are compared.
				request, err := buildStructuredRequest(got, env, schema, "/private-temp/system.txt", input, false)
				if err != nil {
					t.Fatal(err)
				}
				requests = append(requests, probeRequestSnapshot{Model: got, System: system, Input: input, Timeout: structuredProbeTimeout, Request: request})
				var stdin bytes.Buffer
				p, response, err := runStructuredProtocol(strings.NewReader(structuredProbeStream(structuredProbeFinal)), &stdin, schema, input, false)
				want := append(append(slices.Clone(request.Initialize), '\n'), request.Input...)
				want = append(want, '\n')
				if !bytes.Equal(stdin.Bytes(), want) {
					t.Fatal("actual initialize/user writer differs from request projection")
				}
				return p, response, err
			})
		for _, ctx := range contexts {
			if !errors.Is(ctx.Err(), context.Canceled) {
				t.Fatal("generation deadline leaked beyond request lifetime")
			}
		}
		return requests, reports
	}
	one, single := capture(1)
	five, ladder := capture(5)
	if len(one) != 1 || len(five) != 5 || len(single) != 1 || len(ladder) != 5 {
		t.Fatal("offline prefix did not complete")
	}
	if !reflect.DeepEqual(one[0], five[0]) || single[0] != ladder[0] {
		t.Fatal("levels=1 and levels=5 prefix request/outcome differ")
	}
	for i, report := range ladder {
		if report.Level != i+1 || !report.Diagnostic.accepted() {
			t.Fatal("same Final fixture changed outcome across schema levels")
		}
		if five[i].Model != model || five[i].System != one[0].System || five[i].Input != one[0].Input || five[i].Timeout != one[0].Timeout || !reflect.DeepEqual(five[i].Request.Arguments, one[0].Request.Arguments) || !reflect.DeepEqual(five[i].Request.Environment, one[0].Request.Environment) || !bytes.Equal(five[i].Request.Input, one[0].Request.Input) {
			t.Fatal("level data leaked into input, system, model, effort, timeout, argv or env")
		}
	}
	request := one[0].Request
	for _, pair := range [][2]string{{"--model", model.ID}, {"--effort", "high"}, {"--tools", ""}, {"--allowedTools", "StructuredOutput"}, {"--disallowedTools", structuredDeny}, {"--mcp-config", `{"mcpServers":{}}`}, {"--max-turns", "5"}, {"--permission-prompts", "none"}} {
		argument(t, request.Arguments, pair[0], pair[1])
	}
	for _, prohibited := range []string{"--resume", "--continue", "--session-id", "--fork-session", "--fallback-model", "--dangerously-skip-permissions"} {
		if slices.Contains(request.Arguments, prohibited) {
			t.Fatal("probe introduced session continuation or unsafe execution")
		}
	}
	for _, flag := range []string{"--no-session-persistence", "--safe-mode", "--restricted", "--strict-mcp-config"} {
		if !slices.Contains(request.Arguments, flag) {
			t.Fatal("probe lost isolation")
		}
	}
	var init struct {
		Request struct {
			Schema  jsontext.Value `json:"jsonSchema"`
			Hooks   map[string]any `json:"hooks"`
			Servers []string       `json:"sdkMcpServers"`
		} `json:"request"`
	}
	if json.Unmarshal(request.Initialize, &init) != nil || !bytes.Equal(init.Request.Schema, []byte(structuredMinimalProbeSchema)) || init.Request.Hooks == nil || len(init.Request.Hooks) != 0 || init.Request.Servers == nil || len(init.Request.Servers) != 0 {
		t.Fatal("initialize schema/hooks/empty SDK MCP contract changed")
	}
	if single[0].SchemaBytes != 235 || single[0].SchemaSHA256 != structuredProbeSchemaSHA256 {
		t.Fatal("Level 1 schema byte/hash contract changed")
	}
	t.Logf("levels=1 == levels=5 prefix; schema_bytes=%d schema_sha256=%s; prompt/input/system/initialize/argv/env/model/effort/timeout/session options identical", single[0].SchemaBytes, single[0].SchemaSHA256)
}

func TestStructuredLayerProbeFinalOnlyContract(t *testing.T) {
	schema := structuredProbeSchema(t, 1)
	var document struct {
		Type       string         `json:"type"`
		Required   []string       `json:"required"`
		Additional bool           `json:"additionalProperties"`
		OneOf      []any          `json:"oneOf"`
		Properties map[string]any `json:"properties"`
	}
	if json.Unmarshal(schema.document, &document) != nil || document.Type != "object" || document.Additional || !slices.Equal(document.Required, []string{"type", "final"}) || len(document.OneOf) != 0 || len(document.Properties) != 2 || document.Properties["action"] != nil {
		t.Fatal("Level 1 is not the exact Final-only schema")
	}
	for _, raw := range []string{layerRead, `{}`, `{"type":"final","final":{}}`, `{"type":"final","final":{"message":"x","extra":true}}`, `{"type":"final","final":{"message":"x"},"extra":true}`} {
		value, err := jsonschema.UnmarshalJSON(strings.NewReader(raw))
		if err != nil || schema.validator.Validate(value) == nil {
			t.Fatal("Level 1 accepted Action, missing required fields or additional properties")
		}
	}
	value, _ := jsonschema.UnmarshalJSON(strings.NewReader(structuredProbeFinal))
	if schema.validator.Validate(value) != nil {
		t.Fatal("Level 1 rejected its required Final")
	}
}

func TestStructuredLayerProbeOutcomeClassifier(t *testing.T) {
	schema := structuredProbeSchema(t, 3)
	cases := []struct {
		name, stream, outcome, reason, stage string
		present, checked, valid, accepted    bool
	}{
		{"final", structuredProbeStream(structuredProbeFinal), "validated_final", "validated", "", true, true, true, true},
		{"action", structuredProbeStream(layerRead), "validated_action", "unexpected_action", "", true, true, true, false},
		{"different-final-text", structuredProbeStream(layerFinal), "validated_final", "final_message_mismatch", "", true, true, true, false},
		{"missing", structuredProbeStream(""), "structured_missing", "typed_failure", "structured_result_missing_output", false, false, false, false},
		{"null", structuredProbeStream("null"), "structured_null", "typed_failure", "structured_result_null_output", true, false, false, false},
		{"schema", structuredProbeStream(`{"type":"final","final":{}}`), "schema_invalid", "typed_failure", "structured_schema_rejected", true, true, false, false},
		{"envelope", structuredProbeStream(`{"type":"final"}`), "envelope_invalid", "typed_failure", "structured_action_envelope_invalid", true, false, false, false},
		{"unknown-tool", structuredProbeStream(`{"type":"action","action":{"id":"a","tool":"private-probe-unknown-tool","arguments":{}}}`), "envelope_invalid", "typed_failure", "structured_unknown_tool", true, false, false, false},
		{"arguments", structuredProbeStream(`{"type":"action","action":{"id":"a","tool":"read","arguments":{"path":false}}}`), "schema_invalid", "typed_failure", "structured_arguments_schema_rejected", true, true, false, false},
		{"text-only", structuredProbeControl + diagnosticInit + `{"type":"assistant","message":{"model":"synthetic-model","content":[{"type":"text","text":"private-probe-prose"}]}}` + "\n" + `{"type":"result","subtype":"success","result":"private-probe-prose"}` + "\n", "structured_missing", "typed_failure", "structured_result_missing_output", false, false, false, false},
		{"provider-error", structuredProbeControl + diagnosticInit + `{"type":"assistant","error":"unknown","message":{"model":"synthetic-model","content":[{"type":"text","text":"private-probe-prose"}]},"session_id":"private-probe-session"}` + "\n", "provider_failure", "typed_failure", "structured_result_invalid", false, false, false, false},
		{"protocol-failure", structuredProbeControl + diagnosticInit + `{"type":"assistant","message":{"model":"synthetic-model","content":[{"type":"tool_use","id":"private-probe-id","name":"Bash","input":{"command":"private-probe-command"}}]}}` + "\n", "protocol_failure", "typed_failure", "structured_execution_rejected", false, false, false, false},
		{"incomplete", structuredProbeControl + diagnosticInit + structuredProbeSerializer, "protocol_failure", "typed_failure", "structured_stream_incomplete", false, false, false, false},
		{"trailing", structuredProbeStream(structuredProbeFinal) + `{"type":"control_request","request":{"subtype":"private-probe-control"}}` + "\n", "protocol_failure", "typed_failure", "structured_trailing_frame", true, true, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdin bytes.Buffer
			p, response, err := runStructuredProtocol(strings.NewReader(tc.stream), &stdin, schema, structuredProbeInput, false)
			d := classifyStructuredProbe(p, response, err)
			if d.Outcome.String() != tc.outcome || d.Reason.String() != tc.reason || (&Error{structuredStage: d.Stage}).StructuredStage() != tc.stage || d.StructuredOutputPresent != tc.present || d.SchemaChecked != tc.checked || d.SchemaValid != tc.valid || d.accepted() != tc.accepted || d.StructuredOutputNull != (tc.name == "null") {
				t.Fatal("wrong safe outcome", d.summary())
			}
			encoded, _ := json.Marshal(d)
			if strings.Contains(d.summary(), "private-probe-") || bytes.Contains(encoded, []byte("private-probe-")) || len(response.Output) != 0 {
				t.Fatal("provider payload or helper text leaked into probe diagnostic/public output")
			}
			if err != nil && (p.Action != nil || p.Final != nil) {
				t.Fatal("protocol failure exposed an executable proposal")
			}
		})
	}
	for _, tc := range sdkAssistantErrorCases {
		frame, _ := json.Marshal(map[string]any{"type": "assistant", "error": tc.value, "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "private-probe-prose"}}}})
		var stdin bytes.Buffer
		p, response, err := runStructuredProtocol(strings.NewReader(structuredProbeControl+diagnosticInit+string(frame)+"\n"), &stdin, schema, structuredProbeInput, false)
		d := classifyStructuredProbe(p, response, err)
		if d.Outcome != probeProviderFailure || !strings.Contains(d.summary(), "reason="+tc.reason) || d.accepted() {
			t.Fatal("SDK provider failure collapsed into protocol failure", d.summary())
		}
	}
	for _, err := range []error{context.Canceled, context.DeadlineExceeded, errors.New("private-probe-error"), &Error{Code: "private-probe-code"}} {
		d := classifyStructuredProbe(actionProposal{}, llm.Response{}, err)
		encoded, _ := json.Marshal(d)
		if d.accepted() || strings.Contains(d.summary(), "private-probe-") || bytes.Contains(encoded, []byte("private-probe-")) {
			t.Fatal("unclassified error leaked or became success")
		}
	}
	p, err := schema.parse(jsontext.Value(structuredProbeFinal))
	if err != nil {
		t.Fatal(err)
	}
	d := classifyStructuredProbe(p, llm.Response{ID: "private-probe-id", Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "private-probe-prose"}}}}, nil)
	if d.Outcome != probeValidatedFinal || d.Reason != probeUnexpectedResponseOutput || d.accepted() || strings.Contains(d.summary(), "private-probe-") {
		t.Fatal("unexpected public response output indistinguishable from schema failure")
	}
	if d := classifyStructuredProbe(actionProposal{}, llm.Response{}, nil); d.Outcome != probeEnvelopeInvalid || d.accepted() {
		t.Fatal("empty adapter proposal accepted")
	}
}

func TestStructuredLayerProbePreservesSafeMetadataDiagnostic(t *testing.T) {
	stream := structuredProbeControl + diagnosticInit + `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"serializer","name":"StructuredOutput","input":{}}],"future_metadata_field":{"raw":"private-probe-payload"}}}` + "\n"
	var stdin bytes.Buffer
	p, response, err := runStructuredProtocol(strings.NewReader(stream), &stdin, structuredProbeSchema(t, 1), structuredProbeInput, false)
	d := classifyStructuredProbe(p, response, err)
	if d.Outcome != probeProtocolFailure || d.Details == nil || !strings.Contains(d.summary(), "reason=assistant_unexpected_metadata") || !strings.Contains(d.summary(), "metadata_scope=message metadata_field=future_metadata_field") {
		t.Fatal("probe lost the previously established safe parser boundary", d.summary())
	}
	encoded, _ := json.Marshal(d)
	if strings.Contains(d.summary(), "private-probe-payload") || bytes.Contains(encoded, []byte("private-probe-payload")) {
		t.Fatal("safe metadata diagnostic retained a value")
	}
	var e *Error
	if !errors.As(err, &e) || len(d.Details.MessageFields) == 0 {
		t.Fatal("safe shape projection missing")
	}
	d.Details.MessageFields[0].Name = "changed"
	if e.StructuredDetails().MessageFields[0].Name == "changed" {
		t.Fatal("probe diagnostic aliases the adapter's immutable error")
	}
}

func TestStructuredLayerProbeFailureStopsAndStateResets(t *testing.T) {
	model := llm.Model{ID: "synthetic-opus", ReasoningEffort: llm.ReasoningEffortHigh}
	for _, stream := range []string{structuredProbeStream(layerFinal), structuredProbeStream(""), structuredProbeControl + diagnosticInit, structuredProbeStream("null")} {
		run := func(levels int) []structuredProbeReport {
			calls := 0
			reports := runStructuredProbeLevels(t.Context(), levels, model,
				func(level int) *actionSchema { return structuredProbeSchema(t, level) },
				func(_ context.Context, _ llm.Model, schema *actionSchema, _ string, input string) (actionProposal, llm.Response, error) {
					calls++
					var stdin bytes.Buffer
					return runStructuredProtocol(strings.NewReader(stream), &stdin, schema, input, false)
				})
			if calls != 1 || len(reports) != 1 || reports[0].Diagnostic.accepted() {
				t.Fatal("probe advanced/retried after first-level failure")
			}
			return reports
		}
		if !reflect.DeepEqual(run(1), run(5)) {
			t.Fatal("same failing Level 1 fixture changed with maximum level")
		}
	}
	// A previous helper ID may never authorize a later generation's tool_result.
	calls := 0
	reports := runStructuredProbeLevels(t.Context(), 5, model,
		func(level int) *actionSchema { return structuredProbeSchema(t, level) },
		func(_ context.Context, _ llm.Model, schema *actionSchema, _ string, input string) (actionProposal, llm.Response, error) {
			stream := structuredProbeStream(structuredProbeFinal)
			if calls > 0 {
				stream = structuredProbeControl + diagnosticInit + `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"serializer","content":"private-probe-helper"}]}}` + "\n"
			}
			calls++
			var stdin bytes.Buffer
			return runStructuredProtocol(strings.NewReader(stream), &stdin, schema, input, false)
		})
	if len(reports) != 2 || !reports[0].Diagnostic.accepted() || reports[1].Diagnostic.Outcome != probeProtocolFailure || reports[1].Diagnostic.Stage != structuredExecutionRejected {
		t.Fatal("serializer/control/parser state leaked between levels")
	}
	// Schema compilation owns no level-number/global cache, including separate
	// invocations with the same level number. Mutating this disposable fixture
	// cannot authorize tools in a subsequent fresh Final-only schema.
	first, second := structuredProbeSchema(t, 1), structuredProbeSchema(t, 1)
	first.tools["injected"] = true
	first.document[0] = ' '
	if first == second || first.validator == second.validator || second.tools["injected"] || !bytes.Equal(second.document, []byte(structuredMinimalProbeSchema)) {
		t.Fatal("probe schemas share mutable state/cache")
	}
}

func TestStructuredLayerProbeConcurrentPrefixes(t *testing.T) {
	model := llm.Model{ID: "synthetic-opus", ReasoningEffort: llm.ReasoningEffortHigh}
	var wg sync.WaitGroup
	for _, levels := range []int{1, 5, 1, 5} {
		wg.Go(func() {
			reports := runStructuredProbeLevels(t.Context(), levels, model,
				func(level int) *actionSchema { return structuredProbeSchema(t, level) },
				func(_ context.Context, _ llm.Model, schema *actionSchema, _ string, input string) (actionProposal, llm.Response, error) {
					var stdin bytes.Buffer
					return runStructuredProtocol(strings.NewReader(structuredProbeStream(structuredProbeFinal)), &stdin, schema, input, false)
				})
			if len(reports) != levels || !reports[0].Diagnostic.accepted() || reports[0].SchemaSHA256 != structuredProbeSchemaSHA256 {
				t.Error("concurrent probe run contaminated Level 1")
			}
		})
	}
	wg.Wait()
}

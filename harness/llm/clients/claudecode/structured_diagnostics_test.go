//go:build linux || darwin

package claudecode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"math"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

const diagnosticInit = `{"type":"system","subtype":"init","model":"synthetic-model","tools":["StructuredOutput"],"mcp_servers":[],"permissionMode":"default"}` + "\n"
const diagnosticFinal = `{"type":"result","subtype":"success","is_error":false,"structured_output":{"type":"final","final":{"message":"public completion"}}}` + "\n"

func diagnosticSchema(t *testing.T) *actionSchema {
	t.Helper()
	registry := tool.NewRegistry(tool.StaticTranslators{}, tool.ReadName)
	s, err := newActionSchema([]llm.Tool{registry.StaticDefinitions()[0].Tool})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func requireStructuredStage(t *testing.T, err error, stage string) *Error {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) || e.StructuredStage() != stage || !strings.HasPrefix(err.Error(), "stage="+stage+":") {
		t.Fatalf("error=%v; want stage=%s", err, stage)
	}
	d := e.StructuredDetails()
	if d == nil || d.Stage != stage {
		t.Fatal("safe diagnostics missing")
	}
	encoded, marshalErr := json.Marshal(d)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	for _, forbidden := range []string{"private-sensitive", "private-token", "secret-account", "sensitive-argument", "sensitive-prose", "private-helper-id", "private-session-id"} {
		if strings.Contains(err.Error(), forbidden) || bytes.Contains(encoded, []byte(forbidden)) {
			t.Fatal("diagnostic retained a provider value")
		}
	}
	return e
}

func TestStructuredResultValidationStages(t *testing.T) {
	schema := diagnosticSchema(t)
	for _, tc := range []struct{ name, raw, stage, code string }{
		{"missing", "", "structured_result_missing_output", "structured_protocol_invalid"},
		{"null", "null", "structured_result_null_output", "structured_protocol_invalid"},
		{"json", `{"private-token":`, "structured_json_decode_failed", "structured_protocol_invalid"},
		{"duplicate", `{"type":"final","type":"final","final":{"message":"sensitive-prose"}}`, "structured_duplicate_fields", "structured_protocol_invalid"},
		{"trailing", `{"type":"final","final":{"message":"sensitive-prose"}} {}`, "structured_trailing_json", "structured_protocol_invalid"},
		{"wrong-root", `"sensitive-prose"`, "structured_action_envelope_invalid", "structured_protocol_invalid"},
		{"both", `{"type":"action","action":{"id":"a","tool":"read","arguments":{"path":"sensitive-argument"}},"final":{"message":"sensitive-prose"}}`, "structured_action_envelope_invalid", "structured_protocol_invalid"},
		{"neither", `{}`, "structured_action_envelope_invalid", "structured_protocol_invalid"},
		{"envelope-member", `{"type":"final","final":{"message":"sensitive-prose","private-token":"private-sensitive"}}`, "structured_action_envelope_invalid", "structured_protocol_invalid"},
		{"schema", `{"type":"action","action":{"id":"bad/id","tool":"read","arguments":{"path":"sensitive-argument"}}}`, "structured_schema_rejected", "structured_protocol_invalid"},
		{"unknown-tool", `{"type":"action","action":{"id":"a","tool":"private-token","arguments":{}}}`, "structured_unknown_tool", "bridge_unknown_tool"},
		{"arguments-missing", `{"type":"action","action":{"id":"a","tool":"read"}}`, "structured_arguments_schema_rejected", "bridge_arguments_invalid"},
		{"arguments-type", `{"type":"action","action":{"id":"a","tool":"read","arguments":"sensitive-argument"}}`, "structured_arguments_schema_rejected", "bridge_arguments_invalid"},
		{"argument-schema", `{"type":"action","action":{"id":"a","tool":"read","arguments":{"path":true}}}`, "structured_arguments_schema_rejected", "bridge_arguments_invalid"},
		{"oversized", `{"type":"final","final":{"message":"` + strings.Repeat("x", structuredResponseBytes) + `"}}`, "structured_response_oversized", "structured_response_too_large"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := schema.parse(jsontext.Value(tc.raw))
			requireCode(t, err, tc.code)
			e := requireStructuredStage(t, err, tc.stage)
			d := e.StructuredDetails()
			if d.StructuredOutputPresent != (tc.raw != "") || d.StructuredOutputBytes != len(tc.raw) || d.SchemaValid {
				t.Fatal("unsafe/inaccurate output diagnostic")
			}
		})
	}
	for _, raw := range []string{`{"type":"action","action":{"id":"a","tool":"read","arguments":{"path":"synthetic"}}}`, `{"type":"final","final":{"message":"public completion"}}`} {
		if _, err := schema.parse(jsontext.Value(raw)); err != nil {
			t.Fatal("valid public Action/Final rejected", err)
		}
	}
}

func TestStructuredStreamStageBranchesAndSafeShape(t *testing.T) {
	schema := diagnosticSchema(t)
	for _, tc := range []struct{ name, stream, stage string }{
		{"init", strings.Replace(diagnosticInit, `"tools":["StructuredOutput"]`, `"tools":null`, 1), "structured_init_frame_invalid"},
		{"duplicate-init", diagnosticInit + diagnosticInit, "structured_init_frame_invalid"},
		{"serializer-frame", diagnosticInit + `{"type":"assistant","message":{"id":"helper","model":"synthetic","content":[{"type":"tool_use","id":"private-helper-id","name":"StructuredOutput","input":null}]}}`, "structured_serializer_frame_invalid"},
		{"serializer-message", diagnosticInit + `{"type":"assistant","message":{"id":"private-sensitive","model":"","content":[]}}`, "structured_serializer_frame_invalid"},
		{"serializer-partial", diagnosticInit + `{"type":"stream_event","event":{"type":"content_block_start","index":"private-sensitive","content_block":{"type":"tool_use","id":"private-helper-id","name":"StructuredOutput"}}}`, "structured_serializer_partial_invalid"},
		{"partial-without-index", diagnosticInit + `{"type":"stream_event","event":{"type":"content_block_start","content_block":{"type":"tool_use","id":"private-helper-id","name":"StructuredOutput"}}}`, "structured_serializer_partial_invalid"},
		{"result-error", diagnosticInit + `{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["private-sensitive"]}`, "structured_result_invalid"},
		{"result-missing", diagnosticInit + `{"type":"result","subtype":"success","result":"sensitive-prose"}`, "structured_result_missing_output"},
		{"result-null", diagnosticInit + `{"type":"result","subtype":"success","structured_output":null}`, "structured_result_null_output"},
		{"provider-validation-retries", diagnosticInit + `{"type":"result","subtype":"error_max_structured_output_retries","is_error":true,"errors":["private-sensitive"]}`, "structured_schema_rejected"},
		{"unexpected-control", diagnosticInit + `{"type":"control_response","response":{"request_id":"private-sensitive","subtype":"success"}}`, "structured_unexpected_control_frame"},
		{"permission-control", diagnosticInit + `{"type":"control_request","request_id":"private-sensitive","request":{"subtype":"can_use_tool"}}`, "structured_unexpected_control_frame"},
		{"trailing", diagnosticInit + diagnosticFinal + `{"type":"assistant","message":{"id":"private-helper-id","model":"synthetic","content":[{"type":"tool_use","id":"private-helper-id","name":"Bash","input":{"command":"sensitive-argument"}}]}}`, "structured_trailing_frame"},
		{"incomplete", diagnosticInit, "structured_stream_incomplete"},
		{"unfinished-helper", diagnosticInit + `{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"private-helper-id","name":"StructuredOutput","input":{}}}}` + "\n" + diagnosticFinal, "structured_stream_incomplete"},
		{"execution", diagnosticInit + `{"type":"assistant","message":{"id":"helper","model":"synthetic","content":[{"type":"tool_use","id":"private-helper-id","name":"mcp__foreign__read","input":{"path":"sensitive-argument"}}]}}`, "structured_execution_rejected"},
		{"result-execution-field", diagnosticInit + strings.Replace(diagnosticFinal, `"is_error":false`, `"is_error":false,"tool_use_result":{"output":"private-sensitive"}`, 1), "structured_execution_rejected"},
		{"system-metadata", diagnosticInit + `{"type":"system","subtype":"private-sensitive","private-token":"secret-account","message":"sensitive-prose","session_id":"private-session-id"}`, "structured_system_frame_invalid"},
		{"unknown-frame", diagnosticInit + `{"type":"private-sensitive","private-token":"secret-account"}`, "structured_stream_frame_invalid"},
		{"malformed-frame", diagnosticInit + `{"type":[]}`, "structured_stream_frame_invalid"},
		{"permission-denied", diagnosticInit + `{"type":"system","subtype":"permission_denied","message":"sensitive-prose","decision_reason":"private-sensitive","decision_reason_type":"other","session_id":"private-session-id"}`, "structured_permission_denied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := readStructuredStream(bufio.NewScanner(strings.NewReader(tc.stream)), schema, nil, nil)
			e := requireStructuredStage(t, err, tc.stage)
			if tc.name == "system-metadata" {
				d := e.StructuredDetails()
				if d.Frame.Type != "system" || d.Frame.Subtype != "unknown" || !slices.Contains(d.Frame.Fields, StreamField{"message", "string"}) || len(d.Frame.UnknownFields) == 0 {
					t.Fatal("lost safe unknown-system shape")
				}
				// The accessor must not permit UI mutation of the error snapshot.
				d.Frame.Fields[0].Name = "private-sensitive"
				d.ContentBlockTypes = append(d.ContentBlockTypes, "private-sensitive")
				requireStructuredStage(t, e, tc.stage)
			}
		})
	}
}

func TestStructuredPreinitStagesAndNoUserRequest(t *testing.T) {
	schema := diagnosticSchema(t)
	for _, tc := range []struct{ name, stream, stage string }{
		{"malformed-init", `{"type":[]}`, "structured_init_frame_invalid"},
		{"foreign-init", strings.Replace(diagnosticInit, "StructuredOutput", "Bash", 1), "structured_init_frame_invalid"},
		{"preinit-assistant", `{"type":"assistant","message":{"content":[]}}`, "structured_unexpected_preinit_frame"},
		{"init-error", `{"type":"control_response","response":{"subtype":"error","request_id":"unreal_structured_init","error":"private-sensitive"}}`, "structured_init_frame_invalid"},
		{"init-wrong-identity", `{"type":"control_response","response":{"subtype":"success","request_id":"private-sensitive"}}`, "structured_init_frame_invalid"},
		{"incomplete", `{"type":"keep_alive"}` + "\n", "structured_init_incomplete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdin bytes.Buffer
			_, _, err := runStructuredProtocol(strings.NewReader(tc.stream), &stdin, schema, "private-sensitive", false)
			requireStructuredStage(t, err, tc.stage)
			if bytes.Contains(stdin.Bytes(), []byte(`"type":"user"`)) || bytes.Contains(stdin.Bytes(), []byte("private-sensitive")) {
				t.Fatal("failed initialization sent input")
			}
		})
	}
	u := llm.Usage{InputTokens: math.MaxInt64}
	err := addReplayUsage(&u, []llm.Usage{{InputTokens: 1}})
	var e *Error
	if !errors.As(err, &e) || e.StructuredStage() != "structured_usage_invalid" {
		t.Fatal("usage overflow has no safe stage", err)
	}
}

func TestStructuredFinalResultAuthoritativeAcrossHelperRetryAndPartialOrdering(t *testing.T) {
	schema := diagnosticSchema(t)
	// Synthetic values, public pinned SDK frame ordering. Failed helper input
	// and serialization retries are not public execution proposals. Neither
	// partial JSON nor complete assistant input is a durable Host tool request.
	stream := diagnosticInit +
		`{"type":"keep_alive"}` + "\n" +
		`{"type":"stream_event","event":{"type":"message_start","message":{"id":"private-helper-id","model":"synthetic-model","content":[]}}}` + "\n" +
		`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"private-helper-id","name":"StructuredOutput","input":{}}}}` + "\n" +
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"type\":\"final\"}"}}}` + "\n" +
		`{"type":"stream_event","event":{"type":"content_block_stop","index":0}}` + "\n" +
		`{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"tool_use"}}}` + "\n" +
		`{"type":"stream_event","event":{"type":"message_stop"}}` + "\n" +
		`{"type":"assistant","message":{"id":"private-helper-id","model":"synthetic-model","content":[{"type":"thinking","thinking":"private-sensitive"},{"type":"tool_use","id":"private-helper-id","name":"StructuredOutput","input":{"type":"final"}}],"stop_reason":"tool_use"}}` + "\n" +
		`{"type":"tool_progress","tool_use_id":"private-helper-id","tool_name":"StructuredOutput","parent_tool_use_id":null,"elapsed_time_seconds":0.01,"uuid":"private-sensitive","session_id":"private-session-id"}` + "\n" +
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"private-helper-id","is_error":true,"content":"private-sensitive"}]}}` + "\n" +
		`{"type":"assistant","message":{"id":"helper-retry","model":"synthetic-model","content":[{"type":"tool_use","id":"helper-retry","name":"StructuredOutput","input":{"type":"final","final":{"message":"different helper representation"}}}],"stop_reason":"tool_use"}}` + "\n" +
		`{"type":"tool_use_summary","preceding_tool_use_ids":["private-helper-id","helper-retry"],"summary":"private-sensitive","uuid":"private-sensitive","session_id":"private-session-id"}` + "\n" +
		diagnosticFinal + `{"type":"keep_alive"}` + "\n"
	p, r, err := readStructuredStream(bufio.NewScanner(strings.NewReader(stream)), schema, nil, nil)
	if err != nil || p.Type != "final" || p.Final.Message != "public completion" || len(r.Output) != 0 {
		t.Fatal("public result not authoritative", err)
	}
	for _, extra := range []string{
		`{"type":"tool_progress","tool_name":"Bash","tool_use_id":"private-helper-id","elapsed_time_seconds":0}`,
		`{"type":"tool_progress","tool_name":"StructuredOutput","tool_use_id":"foreign","elapsed_time_seconds":0}`,
		`{"type":"tool_progress","tool_name":"StructuredOutput","tool_use_id":"private-helper-id","elapsed_time_seconds":0,"task_id":"private-sensitive"}`,
		`{"type":"tool_use_summary","preceding_tool_use_ids":["private-helper-id","foreign"],"summary":"private-sensitive"}`,
		`{"type":"keep_alive","tool_use_result":{"content":"private-sensitive"}}`,
	} {
		bad := strings.Replace(stream, diagnosticFinal, extra+"\n"+diagnosticFinal, 1)
		if _, _, err := readStructuredStream(bufio.NewScanner(strings.NewReader(bad)), schema, nil, nil); err == nil {
			t.Fatal("uncorrelated execution/heartbeat accepted")
		}
	}
}

func TestStructuredProviderUsesPublicActionNotHelperInput(t *testing.T) {
	c, f, req, opt := structuredClient(t)
	f.Set(t, testclaude.Config{Subscription: "team", BridgeManagedPermissionsOnly: true,
		BridgeSteps:            []testclaude.BridgeStep{{Name: "read", Arguments: `{"path":"fixture.go"}`, Contains: "canonical file receipt", Duplicate: true}},
		StructuredHelperInputs: []string{`{"serializer_internal":{"path":"private-sensitive"}}`, `{}`, `{"type":"final","final":{"message":"sensitive-prose"}}`},
	})
	var calls atomic.Int32
	opt.Tools = func(ctx context.Context, r llm.Response) ([]llm.ToolOutcome, error) {
		calls.Add(1)
		if len(r.Output) != 1 {
			t.Fatal("helper generated extra Host calls")
		}
		call := r.Output[0].Data.(llm.ToolCall)
		if call.Name != "read" || string(call.Arguments) != `{"path":"fixture.go"}` {
			t.Fatal("Host executed helper input")
		}
		return bridgeReply(ctx, r)
	}
	r, err := c.Respond(t.Context(), req, opt)
	if err != nil || calls.Load() != 1 || len(f.Calls(t)) != 3 || r.Output[0].Data.(llm.Message).Text != "Bridge fixture completed" {
		t.Fatal("authoritative Action/receipt/replay/Final failed", err, calls.Load())
	}
	encoded, _ := json.Marshal(r)
	if bytes.Contains(encoded, []byte("private-sensitive")) || bytes.Contains(encoded, []byte("sensitive-prose")) || bytes.Contains(encoded, []byte("StructuredOutput")) {
		t.Fatal("helper protocol leaked to response")
	}
}

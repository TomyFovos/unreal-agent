//go:build linux || darwin

package claudecode

import (
	"bufio"
	"bytes"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func layerCorpus(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "structured", "sdk-0.3.285", name+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func layerReadStream(t *testing.T, data []byte) (actionProposal, error) {
	t.Helper()
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 8192), 2<<20)
	p, response, err := readStructuredStream(scanner, diagnosticSchema(t), nil, nil)
	if len(response.Output) != 0 {
		t.Fatal("helper/protocol became public output")
	}
	encoded, _ := json.Marshal(response)
	for _, private := range []string{"private-sensitive", "private-helper-id", "private-session-id", "dummy-private-summary"} {
		if bytes.Contains(encoded, []byte(private)) {
			t.Fatal("private SDK frame value leaked")
		}
	}
	return p, err
}

func TestStructuredLayerSDKCorpus(t *testing.T) {
	for _, name := range []string{"final", "action", "wire-tool-inputs", "input-transformations"} {
		t.Run(name, func(t *testing.T) {
			data := layerCorpus(t, name)
			p, err := layerReadStream(t, data)
			wantType := name
			if name == "wire-tool-inputs" || name == "input-transformations" {
				wantType = "final"
			}
			if err != nil || p.Type != wantType {
				t.Fatal("pinned SDK corpus rejected", err)
			}
			// initialize/control and normal stdout are separate stages. The
			// transport consumes the init response, never the inference parser.
			protocol := append(layerCorpus(t, "initialize"), data[bytes.IndexByte(data, '\n')+1:]...)
			var stdin bytes.Buffer
			p, _, err = runStructuredProtocol(bytes.NewReader(protocol), &stdin, diagnosticSchema(t), "public fixture task", false)
			if err != nil || p.Type != wantType || bytes.Count(stdin.Bytes(), []byte(`"type":"user"`)) != 1 || bytes.Count(stdin.Bytes(), []byte(`"type":"control_request"`)) != 1 {
				t.Fatal("control/inference separation", err)
			}
		})
	}
}

func TestStructuredLayerSDKRequiredOptionalNullable(t *testing.T) {
	const assistant = `{"type":"assistant","message":` + sdkSerializerMessage + `,"parent_tool_use_id":null,"uuid":"private-helper-id","session_id":"private-session-id"}`
	for _, tc := range []struct {
		name, frame   string
		stage, reason string
	}{
		{"no-subtype", assistant, "", ""},
		{"message-id-absent", assistant, "", ""},
		{"null-stop", strings.Replace(assistant, `"stop_reason":"tool_use"`, `"stop_reason":null`, 1), "", ""},
		{"optional-metadata", strings.Replace(assistant, `"type":"assistant"`, `"type":"assistant","request_id":"private-helper-id","timestamp":"2000-01-01T00:00:00Z","user_message_uuids":[],"supersedes":[]`, 1), "", ""},
		{"wrapper-identity-absent", `{"type":"assistant","message":` + sdkSerializerMessage + `}`, "", ""},
		{"message-absent", `{"type":"assistant"}`, "structured_serializer_frame_invalid", "assistant_message_invalid"},
		{"message-null", `{"type":"assistant","message":null}`, "structured_serializer_frame_invalid", "assistant_message_invalid"},
		{"content-null", strings.Replace(assistant, `"content":[{"type":"tool_use","id":"serializer","name":"StructuredOutput","input":{}}]`, `"content":null`, 1), "structured_serializer_frame_invalid", "assistant_content_invalid"},
		{"nullable-parent-wrong-type", strings.Replace(assistant, `"parent_tool_use_id":null`, `"parent_tool_use_id":42`, 1), "structured_execution_rejected", "assistant_execution_metadata"},
		{"parent-execution", strings.Replace(assistant, `"parent_tool_use_id":null`, `"parent_tool_use_id":"private-helper-id"`, 1), "structured_execution_rejected", "assistant_execution_metadata"},
		{"optional-identity-wrong-type", strings.Replace(assistant, `"uuid":"private-helper-id"`, `"uuid":[]`, 1), "structured_serializer_frame_invalid", "assistant_metadata_invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := layerReadStream(t, []byte(diagnosticInit+tc.frame+"\n"+diagnosticFinal))
			if tc.stage == "" {
				if err != nil || p.Final == nil {
					t.Fatal("legal shape rejected", err)
				}
				return
			}
			e := requireStructuredStage(t, err, tc.stage)
			if e.StructuredDetails().AssistantReason != tc.reason || p.Action != nil || p.Final != nil {
				t.Fatal("unsafe shape reason/proposal")
			}
		})
	}
}

func TestStructuredLayerStreamFailureMatrix(t *testing.T) {
	assistant := func(name string) string {
		return `{"type":"assistant","message":{"model":"synthetic-model","content":[{"type":"tool_use","id":"private-helper-id","name":"` + name + `","input":{}}]}}`
	}
	start := `{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"serializer","name":"StructuredOutput","input":{}}}}` + "\n"
	for _, tc := range []struct{ name, middle, stage, reason string }{
		{"unknown-assistant-field", `{"type":"assistant","foreign_execution":{},"message":` + sdkSerializerMessage + `}`, "structured_serializer_frame_invalid", "assistant_unexpected_metadata"},
		{"malformed-assistant", `{"type":"assistant","message":[]}`, "structured_serializer_frame_invalid", "assistant_message_invalid"},
		{"serializer-input-null", strings.Replace(assistant("StructuredOutput"), `"input":{}`, `"input":null`, 1), "structured_serializer_frame_invalid", "assistant_serializer_input_invalid"},
		{"duplicate-serializer", `{"type":"assistant","message":{"model":"synthetic-model","content":[{"type":"tool_use","id":"same","name":"StructuredOutput","input":{}},{"type":"tool_use","id":"same","name":"StructuredOutput","input":{}}]}}`, "structured_serializer_frame_invalid", "assistant_serializer_duplicate"},
		{"malformed-partial-index", `{"type":"stream_event","event":{"type":"content_block_start","index":null,"content_block":{"type":"text","text":""}}}`, "structured_serializer_partial_invalid", ""},
		{"partial-without-start", `{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{}"}}}`, "structured_serializer_partial_invalid", ""},
		{"stop-without-start", `{"type":"stream_event","event":{"type":"content_block_stop","index":0}}`, "structured_serializer_partial_invalid", ""},
		{"duplicate-partial-start", start + start, "structured_serializer_partial_invalid", ""},
		{"partial-wrong-type", start + `{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":[]}}}`, "structured_serializer_partial_invalid", ""},
		{"partial-oversized", start + `{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"` + strings.Repeat("x", structuredResponseBytes+1) + `"}}}`, "structured_response_oversized", ""},
		{"unexpected-control", `{"type":"control_response","response":{"subtype":"success","request_id":"private-helper-id"}}`, "structured_unexpected_control_frame", ""},
		{"unexpected-permission-control", `{"type":"control_request","request_id":"private-helper-id","request":{"subtype":"can_use_tool"}}`, "structured_unexpected_control_frame", ""},
		{"unknown-system", `{"type":"system","subtype":"unregistered_execution"}`, "structured_system_frame_invalid", ""},
		{"deferred-result-execution", strings.Replace(diagnosticFinal, `"is_error":false`, `"is_error":false,"deferred_tool_use":{"id":"private-helper-id","name":"Bash","input":{}}`, 1), "structured_execution_rejected", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := layerReadStream(t, []byte(diagnosticInit+tc.middle+"\n"+diagnosticFinal))
			e := requireStructuredStage(t, err, tc.stage)
			if e.StructuredDetails().AssistantReason != tc.reason || p.Action != nil || p.Final != nil {
				t.Fatal("unsafe stage/reason/proposal")
			}
		})
	}
	for _, name := range []string{"Bash", "Read", "Write", "Edit", "Glob", "Grep", "Agent", "Task", "Web", "WebFetch", "WebSearch", "mcp__foreign__read", "UnknownTool"} {
		t.Run("execution-"+name, func(t *testing.T) {
			_, err := layerReadStream(t, []byte(diagnosticInit+assistant(name)+"\n"+diagnosticFinal))
			e := requireStructuredStage(t, err, "structured_execution_rejected")
			if e.StructuredDetails().AssistantReason != "assistant_tool_rejected" {
				t.Fatal("wrong closed tool rejection")
			}
		})
	}
	for _, tc := range []struct{ name, stream, stage string }{
		{"missing-result", diagnosticInit, "structured_stream_incomplete"},
		{"unfinished-serializer", diagnosticInit + start + diagnosticFinal, "structured_stream_incomplete"},
		{"trailing-execution", diagnosticInit + diagnosticFinal + assistant("Bash"), "structured_trailing_frame"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := layerReadStream(t, []byte(tc.stream))
			requireStructuredStage(t, err, tc.stage)
		})
	}
	for _, subtype := range []string{"error_during_execution", "error_max_turns", "error_max_budget_usd", "error_max_structured_output_retries"} {
		t.Run(subtype, func(t *testing.T) {
			data := `{"type":"result","subtype":"` + subtype + `","is_error":true,"duration_ms":0,"duration_api_ms":0,"num_turns":0,"stop_reason":null,"total_cost_usd":0,"usage":{},"modelUsage":{},"permission_denials":[],"errors":["private-sensitive"],"uuid":"private-helper-id","session_id":"private-session-id"}`
			_, err := layerReadStream(t, []byte(diagnosticInit+data))
			stage := "structured_result_invalid"
			if subtype == "error_max_structured_output_retries" {
				stage = "structured_schema_rejected"
			}
			if subtype == "error_max_turns" {
				stage = "structured_serializer_round_limit"
			}
			requireStructuredStage(t, err, stage)
		})
	}
}

func TestStructuredLayerControlFailureMatrix(t *testing.T) {
	for _, tc := range []struct{ name, response, stage string }{
		{"wrong-id", `{"subtype":"success","request_id":"private-helper-id"}`, "structured_init_frame_invalid"},
		{"provider-denial", `{"subtype":"error","request_id":"unreal_structured_init","error":"private-sensitive"}`, "structured_init_frame_invalid"},
		{"wrong-response-type", `{"subtype":"success","request_id":"unreal_structured_init","response":null}`, "structured_init_frame_invalid"},
		{"pending-permission", `{"subtype":"success","request_id":"unreal_structured_init","pending_permission_requests":[{}]}`, "structured_unexpected_control_frame"},
		{"pending-dialog", `{"subtype":"success","request_id":"unreal_structured_init","pending_user_dialog_requests":[{}]}`, "structured_unexpected_control_frame"},
		{"wrong-pending-type", `{"subtype":"success","request_id":"unreal_structured_init","pending_permission_requests":null}`, "structured_init_frame_invalid"},
		{"unknown-control-field", `{"subtype":"success","request_id":"unreal_structured_init","private_execution":{}}`, "structured_init_frame_invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var input bytes.Buffer
			_, _, err := runStructuredProtocol(strings.NewReader(`{"type":"control_response","response":`+tc.response+"}\n"), &input, diagnosticSchema(t), "private-sensitive", false)
			requireStructuredStage(t, err, tc.stage)
			if bytes.Contains(input.Bytes(), []byte(`"type":"user"`)) {
				t.Fatal("input sent before initialization authorization")
			}
		})
	}
	for _, response := range []string{`{"subtype":"success","request_id":"unreal_structured_init"}`, `{"subtype":"success","request_id":"unreal_structured_init","response":{},"pending_permission_requests":[],"pending_user_dialog_requests":[]}`} {
		var input bytes.Buffer
		_, _, err := runStructuredProtocol(strings.NewReader(`{"type":"control_response","response":`+response+"}\n"), &input, diagnosticSchema(t), "unused", true)
		if err != nil || bytes.Contains(input.Bytes(), []byte(`"type":"user"`)) {
			t.Fatal("official optional control metadata rejected/probe inferred", err)
		}
	}
}

//go:build linux || darwin

package claudecode

import (
	"bufio"
	"bytes"
	"encoding/json/v2"
	"slices"
	"strings"
	"testing"
)

// Synthetic values only. SDKAssistantMessage has no subtype. The installed
// 2.1.285 BetaMessage wire schema makes id optional and model an opaque string;
// local metadata messages use <synthetic>, never an observed inference model.
const sdkSerializerMessage = `{"role":"assistant","type":"message","model":"<synthetic>","content":[{"type":"tool_use","id":"serializer","name":"StructuredOutput","input":{}}],"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0},"container":null,"context_management":null}`

func TestStructuredAssistantSDKEnvelope(t *testing.T) {
	schema := diagnosticSchema(t)
	for _, tc := range []struct{ name, frame string }{
		{"no-subtype-optional-id", `{"type":"assistant","message":` + sdkSerializerMessage + `}`},
		{"opaque-message-id", `{"type":"assistant","message":` + strings.Replace(sdkSerializerMessage, `"role":`, `"id":"opaque message identity", "role":`, 1) + `}`},
		{"optional-metadata", `{"type":"assistant","message":` + sdkSerializerMessage + `,"parent_tool_use_id":null,"uuid":"private-helper-id","session_id":"private-session-id","request_id":"private-sensitive","timestamp":"2000-01-01T00:00:00Z","user_message_uuid":"private-token","user_message_uuids":["private-token"],"resumed_from_incomplete_thinking":true}`},
		{"streamed-block-null-stop", `{"type":"assistant","message":` + strings.Replace(sdkSerializerMessage, `"stop_reason":"tool_use"`, `"stop_reason":null`, 1) + `}`},
		{"direct-caller-display-metadata", `{"type":"assistant","message":` + strings.Replace(sdkSerializerMessage, `"input":{}`, `"input":{},"caller":{"type":"direct"}`, 1) + `,"tool_use_meta":[{"id":"serializer","display_name":"private-sensitive"}]}`},
		{"API-model-observation", `{"type":"assistant","message":` + strings.Replace(sdkSerializerMessage, `<synthetic>`, `synthetic-model`, 1) + `}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proposal, response, err := readStructuredStream(bufio.NewScanner(strings.NewReader(diagnosticInit+tc.frame+"\n"+diagnosticFinal)), schema, nil, nil)
			if err != nil {
				t.Fatal("legal SDK serializer envelope rejected", err)
			}
			if proposal.Final == nil || proposal.Final.Message != "public completion" || response.Model != "synthetic-model" || response.ID != "" || len(response.Output) != 0 {
				t.Fatal("private helper metadata became public identity/content", proposal, response)
			}
		})
	}
}

func TestStructuredAssistantRejectsExecutionAndMalformedContent(t *testing.T) {
	schema := diagnosticSchema(t)
	frame := func(message string) string { return `{"type":"assistant","message":` + message + `}` }
	withBlock := func(block string) string {
		return frame(strings.Replace(sdkSerializerMessage, `"content":[`, `"content":[`+block+`,`, 1))
	}
	for _, tc := range []struct{ name, frame, reason, stage string }{
		{"Bash", withBlock(`{"type":"tool_use","id":"private-helper-id","name":"Bash","input":{"command":"sensitive-argument"}}`), "assistant_tool_rejected", "structured_execution_rejected"},
		{"Read", withBlock(`{"type":"tool_use","id":"private-helper-id","name":"Read","input":{"file_path":"sensitive-argument"}}`), "assistant_tool_rejected", "structured_execution_rejected"},
		{"Edit", withBlock(`{"type":"tool_use","id":"private-helper-id","name":"Edit","input":{}}`), "assistant_tool_rejected", "structured_execution_rejected"},
		{"Agent", withBlock(`{"type":"tool_use","id":"private-helper-id","name":"Agent","input":{}}`), "assistant_tool_rejected", "structured_execution_rejected"},
		{"foreign-MCP", withBlock(`{"type":"tool_use","id":"private-helper-id","name":"mcp__foreign__private-token","input":{}}`), "assistant_tool_rejected", "structured_execution_rejected"},
		{"unknown-tool", withBlock(`{"type":"tool_use","id":"private-helper-id","name":"private-token","input":{}}`), "assistant_tool_rejected", "structured_execution_rejected"},
		{"server-tool", withBlock(`{"type":"server_tool_use","id":"private-helper-id","name":"private-token","input":{}}`), "assistant_block_rejected", "structured_execution_rejected"},
		{"MCP-block", withBlock(`{"type":"mcp_tool_use","id":"private-helper-id","name":"private-token","input":{}}`), "assistant_block_rejected", "structured_execution_rejected"},
		{"missing-message", `{"type":"assistant"}`, "assistant_message_invalid", "structured_serializer_frame_invalid"},
		{"null-message", frame("null"), "assistant_message_invalid", "structured_serializer_frame_invalid"},
		{"wrong-message-type", frame(`"private-sensitive"`), "assistant_message_invalid", "structured_serializer_frame_invalid"},
		{"null-content", frame(`{"model":"<synthetic>","content":null}`), "assistant_content_invalid", "structured_serializer_frame_invalid"},
		{"missing-content", frame(`{"model":"<synthetic>"}`), "assistant_content_invalid", "structured_serializer_frame_invalid"},
		{"wrong-content-type", frame(`{"model":"<synthetic>","content":"private-sensitive"}`), "assistant_content_invalid", "structured_serializer_frame_invalid"},
		{"wrong-id-type", frame(strings.Replace(sdkSerializerMessage, `"role":`, `"id":{},"role":`, 1)), "assistant_id_invalid", "structured_serializer_frame_invalid"},
		{"missing-model", frame(`{"content":[]}`), "assistant_model_invalid", "structured_serializer_frame_invalid"},
		{"wrong-role", frame(strings.Replace(sdkSerializerMessage, `"role":"assistant"`, `"role":"user"`, 1)), "assistant_role_invalid", "structured_serializer_frame_invalid"},
		{"wrong-message-tag", frame(strings.Replace(sdkSerializerMessage, `"type":"message"`, `"type":"private-sensitive"`, 1)), "assistant_message_type_invalid", "structured_serializer_frame_invalid"},
		{"unknown-stop", frame(strings.Replace(sdkSerializerMessage, `"stop_reason":"tool_use"`, `"stop_reason":"private-sensitive"`, 1)), "assistant_stop_invalid", "structured_serializer_frame_invalid"},
		{"null-block", withBlock(`null`), "assistant_block_invalid", "structured_serializer_frame_invalid"},
		{"wrong-text", withBlock(`{"type":"text","text":42}`), "assistant_block_invalid", "structured_serializer_frame_invalid"},
		{"missing-text", withBlock(`{"type":"text"}`), "assistant_block_invalid", "structured_serializer_frame_invalid"},
		{"wrong-thinking", withBlock(`{"type":"thinking","thinking":[]}`), "assistant_block_invalid", "structured_serializer_frame_invalid"},
		{"wrong-signature", withBlock(`{"type":"thinking","thinking":"private-sensitive","signature":42}`), "assistant_block_invalid", "structured_serializer_frame_invalid"},
		{"wrong-redacted", withBlock(`{"type":"redacted_thinking","data":42}`), "assistant_block_invalid", "structured_serializer_frame_invalid"},
		{"null-helper-input", frame(strings.Replace(sdkSerializerMessage, `"input":{}`, `"input":null`, 1)), "assistant_serializer_input_invalid", "structured_serializer_frame_invalid"},
		{"missing-helper-input", frame(strings.Replace(sdkSerializerMessage, `,"input":{}`, ``, 1)), "assistant_serializer_input_invalid", "structured_serializer_frame_invalid"},
		{"wrong-helper-id", frame(strings.Replace(sdkSerializerMessage, `"id":"serializer"`, `"id":null`, 1)), "assistant_serializer_id_invalid", "structured_serializer_frame_invalid"},
		{"server-caller", frame(strings.Replace(sdkSerializerMessage, `"input":{}`, `"input":{},"caller":{"type":"server_tool","tool_id":"private-helper-id"}`, 1)), "assistant_execution_metadata", "structured_execution_rejected"},
		{"container", frame(strings.Replace(sdkSerializerMessage, `"container":null`, `"container":{"id":"private-helper-id"}`, 1)), "assistant_execution_metadata", "structured_execution_rejected"},
		{"server-usage", frame(strings.Replace(sdkSerializerMessage, `"output_tokens":0`, `"output_tokens":0,"server_tool_use":{"web_search_requests":1,"web_fetch_requests":0}`, 1)), "assistant_execution_metadata", "structured_execution_rejected"},
		{"foreign-display-metadata", `{"type":"assistant","message":` + sdkSerializerMessage + `,"tool_use_meta":[{"id":"serializer","display_name":"private-sensitive","server_display_name":"private-token"}]}`, "assistant_execution_metadata", "structured_execution_rejected"},
		{"execution-result-field", `{"type":"assistant","message":` + sdkSerializerMessage + `,"tool_use_result":{"output":"private-sensitive"}}`, "assistant_unexpected_metadata", "structured_serializer_frame_invalid"},
		{"subagent", `{"type":"assistant","message":` + sdkSerializerMessage + `,"parent_tool_use_id":"private-helper-id"}`, "assistant_execution_metadata", "structured_execution_rejected"},
		{"unknown-subtype", `{"type":"assistant","subtype":"private-sensitive","message":` + sdkSerializerMessage + `}`, "assistant_subtype_unsupported", "structured_serializer_frame_invalid"},
		{"wrong-usage", frame(strings.Replace(sdkSerializerMessage, `"usage":{"input_tokens":0,"output_tokens":0}`, `"usage":[]`, 1)), "assistant_usage_invalid", "structured_serializer_frame_invalid"},
		{"usage-decode", frame(strings.Replace(sdkSerializerMessage, `"input_tokens":0`, `"input_tokens":"private-token"`, 1)), "assistant_decode_invalid", "structured_serializer_frame_invalid"},
		{"wrong-metadata", `{"type":"assistant","message":` + sdkSerializerMessage + `,"timestamp":{}}`, "assistant_metadata_invalid", "structured_serializer_frame_invalid"},
		{"wrong-error", `{"type":"assistant","message":` + sdkSerializerMessage + `,"error":{}}`, "assistant_error_invalid", "structured_serializer_frame_invalid"},
		{"aborted", `{"type":"assistant","message":` + sdkSerializerMessage + `,"aborted":true}`, "assistant_aborted", "structured_stream_incomplete"},
		{"provider-error", `{"type":"assistant","message":` + sdkSerializerMessage + `,"error":"authentication_failed"}`, "assistant_authentication_failed", "structured_result_invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proposal, response, err := readStructuredStream(bufio.NewScanner(strings.NewReader(diagnosticInit+tc.frame+"\n"+diagnosticFinal)), schema, nil, nil)
			e := requireStructuredStage(t, err, tc.stage)
			if e.StructuredDetails().AssistantReason != tc.reason || proposal.Action != nil || proposal.Final != nil || len(response.Output) != 0 {
				t.Fatal("wrong rejection boundary or premature proposal", e.StructuredDetails().AssistantReason)
			}
		})
	}
}

func TestStructuredAssistantDiagnosticIsSafeAndDisposable(t *testing.T) {
	frame := `{"type":"assistant","message":{"model":"private-token","content":[{"type":"tool_use","id":"private-helper-id","name":"StructuredOutput","input":null}],"future_metadata_field":"secret-account"},"session_id":"private-session-id"}`
	_, _, err := readStructuredStream(bufio.NewScanner(strings.NewReader(diagnosticInit+frame)), diagnosticSchema(t), nil, nil)
	e := requireStructuredStage(t, err, "structured_serializer_frame_invalid")
	d := e.StructuredDetails()
	if d.Frame.Type != "assistant" || d.Frame.Subtype != "unknown" || d.Frame.SubtypePresent || d.ContentBlockCount != 1 || !slices.Equal(d.ToolNameKinds, []string{"StructuredOutput"}) || len(d.MessageFields) != 2 || len(d.UnknownMessageFields) != 1 || !d.SerializerInputPresent {
		t.Fatal("safe shape missing", d)
	}
	if d.UnexpectedMetadataScope != "message" || d.UnexpectedMetadataField != "future_metadata_field" {
		t.Fatal("rejected message member name missing")
	}
	d.MessageFields[0].Name = "private-token"
	d.ToolNameKinds[0] = "private-sensitive"
	d.UnknownMessageFields[0].Count = 999
	d.UnexpectedMetadataField = "private-sensitive"
	encoded, _ := json.Marshal(e.StructuredDetails())
	for _, private := range []string{"private-token", "private-sensitive", "private-helper-id", "private-session-id", "secret-account", "999"} {
		if bytes.Contains(encoded, []byte(private)) || strings.Contains(e.Error(), private) {
			t.Fatal("mutable or value-bearing error projection")
		}
	}
}

func TestStructuredMultipleSerializerBlocksRemainOneAuthoritativeProposal(t *testing.T) {
	// BetaMessage.content is an array. The SDK allows serializer retries; there
	// is no public one-helper-block limit. These are serialization, not actions.
	message := strings.Replace(sdkSerializerMessage, `"content":[`, `"content":[{"type":"tool_use","id":"serializer-retry","name":"StructuredOutput","input":{"private_internal":"private-sensitive"}},`, 1)
	stream := diagnosticInit + `{"type":"assistant","message":` + message + `}` + "\n" + diagnosticFinal
	proposal, response, err := readStructuredStream(bufio.NewScanner(strings.NewReader(stream)), diagnosticSchema(t), nil, nil)
	if err != nil || proposal.Action != nil || proposal.Final == nil || proposal.Final.Message != "public completion" || len(response.Output) != 0 {
		t.Fatal("serializer helpers became Actions", err)
	}
}

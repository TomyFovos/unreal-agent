//go:build linux || darwin

package claudecode

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestStructuredAssistantWireInputsObservedBoundary(t *testing.T) {
	for _, name := range []string{"wire-tool-inputs", "wire-tool-inputs-message-unknown"} {
		t.Run(name, func(t *testing.T) {
			p, err := layerReadStream(t, layerCorpus(t, name))
			if name == "wire-tool-inputs" {
				if err != nil || p.Action != nil || p.Final == nil || p.Final.Message != "public completion" {
					t.Fatal("installed raw-input metadata rejected or promoted to a proposal", err)
				}
				return
			}
			e := requireStructuredStage(t, err, "structured_serializer_frame_invalid")
			d := e.StructuredDetails()
			if d.AssistantReason != "assistant_unexpected_metadata" || d.UnexpectedMetadataScope != "message" ||
				d.UnexpectedMetadataField != "private-unrecognized-message-key" || d.AssistantUnknownFieldsPresent || d.MessageUnknownFieldsPresent ||
				d.ContentBlockCount != 1 || d.StructuredOutputBlocks != 1 || !d.SerializerInputPresent ||
				d.PartialEventType != "content_block_delta" || d.DeltaType != "input_json_delta" ||
				p.Action != nil || p.Final != nil {
				t.Fatal("unresolved message metadata was accepted or obscured")
			}
			if !slices.Contains(d.Frame.Fields, StreamField{"wire_tool_inputs", "object"}) || len(d.Frame.UnknownFields) != 0 ||
				!slices.Equal(d.UnknownMessageFields, []StreamFieldCount{{JSONType: "array", Count: 1}}) ||
				!strings.Contains(e.Error(), "metadata_scope=message metadata_field=private-unrecognized-message-key ") {
				t.Fatal("wire presence/type or remaining unknown message count was misreported/leaked")
			}
		})
	}
}

func wireFixtureFrame(t *testing.T) (map[string]jsontext.Value, map[string]jsontext.Value) {
	t.Helper()
	var frame, message map[string]jsontext.Value
	line := bytes.Split(layerCorpus(t, "wire-tool-inputs"), []byte("\n"))[3]
	if json.Unmarshal(line, &frame) != nil || json.Unmarshal(frame["message"], &message) != nil {
		t.Fatal("wire fixture malformed")
	}
	return frame, message
}

func TestStructuredAssistantWireInputsLegalAndOpaque(t *testing.T) {
	for _, tc := range []struct{ name, wire string }{
		{"absent", ""}, {"empty", `{}`}, {"minimal", `{"serializer":{}}`},
		{"raw-normalized-differ", `{"serializer":{"type":"final","final":{"message":"private-wire-payload"}}}`},
		{"action-proposal-is-only-data", `{"serializer":{"type":"action","action":{"id":"wire-private-action","tool":"Bash","arguments":{"command":"private-wire-payload"}}}}`},
		{"ordinary-nested-json", `{"serializer":{"nested":[null,true,42,{},[],"private-wire-payload"]}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frame, message := wireFixtureFrame(t)
			if tc.wire == "" {
				delete(frame, "wire_tool_inputs")
			} else {
				frame["wire_tool_inputs"] = jsontext.Value(tc.wire)
			}
			data := metadataFixtureStream(t, frame, message)
			p, err := layerReadStream(t, data)
			if err != nil || p.Action != nil || p.Final == nil || p.Final.Message != "public completion" {
				t.Fatal("legal metadata changed the authoritative result", err)
			}
			var d StructuredDiagnostics
			line := bytes.Split(data, []byte("\n"))[3]
			record, err := structuredAssistant(line, &d)
			if err != nil {
				t.Fatal(err)
			}
			public, _ := json.Marshal(record)
			if bytes.Contains(public, []byte("wire_tool_inputs")) || bytes.Contains(public, []byte("private-wire-payload")) {
				t.Fatal("private transport data survived normalization")
			}
			result := bytes.Index(data, []byte(`{"type":"result"`))
			p, err = layerReadStream(t, data[:result])
			requireStructuredStage(t, err, "structured_stream_incomplete")
			if p.Action != nil || p.Final != nil {
				t.Fatal("wire/helper metadata substituted for missing structured_output")
			}
		})
	}
}

func TestStructuredAssistantWireInputsRejectsMalformedAndInjected(t *testing.T) {
	many := map[string]any{}
	for i := 0; i <= structuredWireInputEntries; i++ {
		many[fmt.Sprintf("dummy-%d", i)] = map[string]any{}
	}
	manyJSON, _ := json.Marshal(many)
	deep := strings.Repeat(`{"nested":`, structuredWireInputDepth) + `{}` + strings.Repeat(`}`, structuredWireInputDepth)
	nodes := `{"serializer":{"nested":[` + strings.Repeat(`null,`, structuredWireInputValues) + `null]}}`
	for _, tc := range []struct{ name, raw, stage, reason string }{
		{"null", `null`, "structured_serializer_frame_invalid", "assistant_wire_tool_inputs_invalid"},
		{"array", `[]`, "structured_serializer_frame_invalid", "assistant_wire_tool_inputs_invalid"},
		{"string", `"private-wire-payload"`, "structured_serializer_frame_invalid", "assistant_wire_tool_inputs_invalid"},
		{"number", `42`, "structured_serializer_frame_invalid", "assistant_wire_tool_inputs_invalid"},
		{"boolean", `true`, "structured_serializer_frame_invalid", "assistant_wire_tool_inputs_invalid"},
		{"entry-null", `{"serializer":null}`, "structured_serializer_frame_invalid", "assistant_wire_tool_inputs_invalid"},
		{"entry-string", `{"serializer":"private-wire-payload"}`, "structured_serializer_frame_invalid", "assistant_wire_tool_inputs_invalid"},
		{"entry-array", `{"serializer":[]}`, "structured_serializer_frame_invalid", "assistant_wire_tool_inputs_invalid"},
		{"unrelated-id", `{"private-helper-id":{}}`, "structured_serializer_frame_invalid", "assistant_wire_tool_inputs_invalid"},
		{"count-bound", string(manyJSON), "structured_serializer_frame_invalid", "assistant_wire_tool_inputs_invalid"},
		{"depth-bound", `{"serializer":` + deep + `}`, "structured_serializer_frame_invalid", "assistant_wire_tool_inputs_invalid"},
		{"value-bound", nodes, "structured_serializer_frame_invalid", "assistant_wire_tool_inputs_invalid"},
		{"size-bound", `{"serializer":{"value":"` + strings.Repeat("x", structuredWireInputBytes) + `"}}`, "structured_response_oversized", "assistant_wire_tool_inputs_oversized"},
		{"duplicate-entry", `{"serializer":{},"serializer":{}}`, "structured_duplicate_fields", ""},
		{"duplicate-nested", `{"serializer":{"value":1,"value":2}}`, "structured_duplicate_fields", ""},
		{"malformed-nested-json", `{"serializer":{"nested":[}}`, "structured_json_decode_failed", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frame, message := wireFixtureFrame(t)
			// Assemble invalid/duplicate JSON verbatim: the transport, not a
			// fixture encoder that fixes/drops fields, must reject it.
			frame["wire_tool_inputs"] = jsontext.Value(`"replace-wire-value"`)
			data := bytes.Replace(metadataFixtureStream(t, frame, message), []byte(`"replace-wire-value"`), []byte(tc.raw), 1)
			p, err := layerReadStream(t, data)
			e := requireStructuredStage(t, err, tc.stage)
			if e.StructuredDetails().AssistantReason != tc.reason || p.Action != nil || p.Final != nil {
				t.Fatal("malformed transport input changed classification or generated a proposal")
			}
			encoded, _ := json.Marshal(e.StructuredDetails())
			if bytes.Contains(encoded, []byte("private-wire-payload")) || strings.Contains(e.Error(), "private-wire-payload") {
				t.Fatal("transport body leaked")
			}
		})
	}
	for _, tag := range []string{"tool_use", "server_tool_use", "mcp_tool_use", "mcp_tool_result", "tool_result", "tool_progress", "tool_use_summary", "control_request", "control_response", "task_started", "task_progress", "task_notification", "task_updated"} {
		t.Run("injected-"+tag, func(t *testing.T) {
			frame, message := wireFixtureFrame(t)
			frame["wire_tool_inputs"] = jsontext.Value(`{"serializer":{"nested":[{"type":"` + tag + `","name":"Bash","input":{"command":"private-wire-payload"}}]}}`)
			p, err := layerReadStream(t, metadataFixtureStream(t, frame, message))
			e := requireStructuredStage(t, err, "structured_serializer_frame_invalid")
			if e.StructuredDetails().AssistantReason != "assistant_wire_tool_inputs_invalid" || p.Action != nil || p.Final != nil || strings.Contains(e.Error(), "private-wire-payload") {
				t.Fatal("execution frame in wire metadata was accepted or leaked")
			}
		})
	}
}

func TestStructuredAssistantWireInputsResourceBoundaries(t *testing.T) {
	frame, message := wireFixtureFrame(t)
	var content []map[string]any
	inputs := map[string]any{}
	for i := 0; i < structuredWireInputEntries; i++ {
		id := fmt.Sprintf("serializer-%d", i)
		content = append(content, map[string]any{"type": "tool_use", "id": id, "name": structuredOutputTool, "input": map[string]any{}})
		inputs[id] = map[string]any{}
	}
	message["content"], _ = json.Marshal(content)
	frame["wire_tool_inputs"], _ = json.Marshal(inputs)
	var d StructuredDiagnostics
	line := bytes.Split(metadataFixtureStream(t, frame, message), []byte("\n"))[3]
	if _, err := structuredAssistant(line, &d); err != nil {
		t.Fatal("exact entry count boundary rejected", err)
	}
	deep := strings.Repeat(`{"nested":`, structuredWireInputDepth-1) + `{}` + strings.Repeat(`}`, structuredWireInputDepth-1)
	if _, ok := serializerWireInputs(jsontext.Value(`{"serializer":` + deep + `}`)); !ok {
		t.Fatal("exact nesting boundary rejected")
	}
}

func TestStructuredAssistantWireInputsKeepToolAndSiblingIsolation(t *testing.T) {
	for _, name := range []string{"Bash", "Read", "Write", "Edit", "Glob", "Grep", "Agent", "Task", "Web", "WebFetch", "WebSearch", "mcp__foreign__read", "UnknownTool"} {
		t.Run(name, func(t *testing.T) {
			data := bytes.ReplaceAll(layerCorpus(t, "wire-tool-inputs"), []byte(`"name":"StructuredOutput"`), []byte(`"name":"`+name+`"`))
			p, err := layerReadStream(t, data)
			e := requireStructuredStage(t, err, "structured_execution_rejected")
			if e.StreamReason() != "stream_tool_execution" || p.Action != nil || p.Final != nil {
				t.Fatal("wire metadata authorized an unowned tool")
			}
		})
	}
	for _, scope := range []string{"assistant", "message"} {
		t.Run("unknown-sibling-"+scope, func(t *testing.T) {
			frame, message := wireFixtureFrame(t)
			if scope == "assistant" {
				frame["private-unknown-field"] = jsontext.Value(`{}`)
			} else {
				message["private-unknown-field"] = jsontext.Value(`[]`)
			}
			p, err := layerReadStream(t, metadataFixtureStream(t, frame, message))
			e := requireStructuredStage(t, err, "structured_serializer_frame_invalid")
			if e.StructuredDetails().AssistantReason != "assistant_unexpected_metadata" || e.StructuredDetails().UnexpectedMetadataScope != scope || p.Action != nil || p.Final != nil {
				t.Fatal("unknown sibling escaped the exact-field exception")
			}
		})
	}
}

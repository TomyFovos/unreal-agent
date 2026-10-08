//go:build linux || darwin

package claudecode

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"slices"
	"strings"
	"testing"
)

func TestStructuredAssistantInputTransformationsObservedBoundary(t *testing.T) {
	p, err := layerReadStream(t, layerCorpus(t, "input-transformations"))
	if err != nil || p.Action != nil || p.Final == nil || p.Final.Message != "public completion" {
		t.Fatal("installed input transformation metadata rejected or became a proposal", err)
	}
}

func transformationFixtureFrame(t *testing.T) (map[string]jsontext.Value, map[string]jsontext.Value) {
	t.Helper()
	var frame, message map[string]jsontext.Value
	line := bytes.Split(layerCorpus(t, "input-transformations"), []byte("\n"))[3]
	if json.Unmarshal(line, &frame) != nil || json.Unmarshal(frame["message"], &message) != nil {
		t.Fatal("transformation fixture malformed")
	}
	return frame, message
}

func TestStructuredAssistantInputTransformationsLegalAndDiscarded(t *testing.T) {
	for _, tc := range []struct{ name, raw string }{
		{"absent", ""}, {"empty", `[]`},
		{"minimal", `[{"type":"thinking_dropped","path":"","reason":""}]`},
		{"prefix", `[{"type":"thinking_dropped","path":"messages.0.content.0","reason":"prefix_binding_mismatch"}]`},
		{"model", `[{"type":"thinking_dropped","path":"messages.1","reason":"model_binding_mismatch"}]`},
		{"opaque-values", `[{"type":"thinking_dropped","path":"private-transform-path","reason":"private-transform-reason"}]`},
		{"multiple", `[{"type":"thinking_dropped","path":"messages.0.content.0","reason":"prefix_binding_mismatch"},{"type":"thinking_dropped","path":"messages.1","reason":"other"}]`},
		{"execution-text-is-only-data", `[{"type":"thinking_dropped","path":"private-transform-path","reason":"{\"type\":\"action\",\"action\":{\"id\":\"private-action-id\",\"tool\":\"Bash\",\"arguments\":{\"command\":\"private-transform-command\"}}}"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frame, message := transformationFixtureFrame(t)
			if tc.raw == "" {
				delete(message, "input_transformations")
			} else {
				message["input_transformations"] = jsontext.Value(tc.raw)
			}
			data := metadataFixtureStream(t, frame, message)
			p, err := layerReadStream(t, data)
			if err != nil || p.Action != nil || p.Final == nil || p.Final.Message != "public completion" {
				t.Fatal("input metadata altered authoritative result", err)
			}
			var d StructuredDiagnostics
			line := bytes.Split(data, []byte("\n"))[3]
			record, err := structuredAssistant(line, &d)
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(record)
			if bytes.Contains(encoded, []byte("input_transformations")) || bytes.Contains(encoded, []byte("private-transform")) || bytes.Contains(encoded, []byte("thinking_dropped")) {
				t.Fatal("private transformation values survived normalization")
			}
			end := bytes.Index(data, []byte(`{"type":"result"`))
			p, err = layerReadStream(t, data[:end])
			requireStructuredStage(t, err, "structured_stream_incomplete")
			if p.Action != nil || p.Final != nil {
				t.Fatal("transformation or serializer data substituted for structured_output")
			}
		})
	}
}

func TestStructuredAssistantInputTransformationsRejectMalformedAndBounded(t *testing.T) {
	entry := `{"type":"thinking_dropped","path":"messages.0.content.0","reason":"private-transform-reason"}`
	deep := strings.Repeat(`{"nested":`, 40) + `"private-transform-value"` + strings.Repeat(`}`, 40)
	for _, tc := range []struct{ name, raw, stage, reason string }{
		{"null", `null`, "structured_serializer_frame_invalid", "assistant_input_transformations_invalid"},
		{"object", `{}`, "structured_serializer_frame_invalid", "assistant_input_transformations_invalid"},
		{"string", `"private-transform-value"`, "structured_serializer_frame_invalid", "assistant_input_transformations_invalid"},
		{"number", `42`, "structured_serializer_frame_invalid", "assistant_input_transformations_invalid"},
		{"boolean", `true`, "structured_serializer_frame_invalid", "assistant_input_transformations_invalid"},
		{"entry-null", `[null]`, "structured_serializer_frame_invalid", "assistant_input_transformations_invalid"},
		{"entry-array", `[[]]`, "structured_serializer_frame_invalid", "assistant_input_transformations_invalid"},
		{"entry-string", `["private-transform-value"]`, "structured_serializer_frame_invalid", "assistant_input_transformations_invalid"},
		{"missing-type", `[{"path":"private-transform-path","reason":"private-transform-reason"}]`, "structured_serializer_frame_invalid", "assistant_input_transformations_invalid"},
		{"missing-path", `[{"type":"thinking_dropped","reason":"private-transform-reason"}]`, "structured_serializer_frame_invalid", "assistant_input_transformations_invalid"},
		{"missing-reason", `[{"type":"thinking_dropped","path":"private-transform-path"}]`, "structured_serializer_frame_invalid", "assistant_input_transformations_invalid"},
		{"wrong-type-tag", `[{"type":"private-transform-kind","path":"private-transform-path","reason":"private-transform-reason"}]`, "structured_serializer_frame_invalid", "assistant_input_transformations_invalid"},
		{"wrong-path-type", `[{"type":"thinking_dropped","path":{},"reason":"private-transform-reason"}]`, "structured_serializer_frame_invalid", "assistant_input_transformations_invalid"},
		{"wrong-reason-type", `[{"type":"thinking_dropped","path":"private-transform-path","reason":[]}]`, "structured_serializer_frame_invalid", "assistant_input_transformations_invalid"},
		{"null-reason", `[{"type":"thinking_dropped","path":"private-transform-path","reason":null}]`, "structured_serializer_frame_invalid", "assistant_input_transformations_invalid"},
		{"unknown-entry-field", `[{"type":"thinking_dropped","path":"private-transform-path","reason":"private-transform-reason","private-transform-field":{}}]`, "structured_serializer_frame_invalid", "assistant_input_transformations_invalid"},
		{"depth", `[{"type":"thinking_dropped","path":"private-transform-path","reason":` + deep + `}]`, "structured_serializer_frame_invalid", "assistant_input_transformations_invalid"},
		{"count", `[` + strings.Repeat(entry+`,`, structuredInputTransformationEntries) + entry + `]`, "structured_serializer_frame_invalid", "assistant_input_transformations_invalid"},
		{"size", `[{"type":"thinking_dropped","path":"` + strings.Repeat("x", structuredInputTransformationBytes) + `","reason":""}]`, "structured_response_oversized", "assistant_input_transformations_oversized"},
		{"duplicate-field", `[{"type":"thinking_dropped","path":"private-transform-path","path":"private-transform-path","reason":""}]`, "structured_duplicate_fields", ""},
		{"malformed-json", `[{"type":"thinking_dropped","path":"private-transform-path","reason":}]`, "structured_json_decode_failed", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frame, message := transformationFixtureFrame(t)
			message["input_transformations"] = jsontext.Value(`"replace-transform-value"`)
			data := bytes.Replace(metadataFixtureStream(t, frame, message), []byte(`"replace-transform-value"`), []byte(tc.raw), 1)
			p, err := layerReadStream(t, data)
			e := requireStructuredStage(t, err, tc.stage)
			d := e.StructuredDetails()
			if d.AssistantReason != tc.reason || p.Action != nil || p.Final != nil {
				t.Fatal("invalid metadata altered its rejection boundary or became work")
			}
			if tc.reason != "" && (d.UnexpectedMetadataScope != "message" || d.UnexpectedMetadataField != "input_transformations") {
				t.Fatal("invalid metadata location missing")
			}
			encoded, _ := json.Marshal(d)
			for _, private := range []string{"private-transform", "private-wire-payload", "private-helper-id", "private-session-id"} {
				if bytes.Contains(encoded, []byte(private)) || strings.Contains(e.Error(), private) {
					t.Fatal("raw metadata or helper value leaked")
				}
			}
		})
	}
}

func TestStructuredAssistantInputTransformationsResourceBoundaries(t *testing.T) {
	entry := `{"type":"thinking_dropped","path":"","reason":""}`
	if !validInputTransformations(jsontext.Value(`[` + strings.Repeat(entry+`,`, structuredInputTransformationEntries-1) + entry + `]`)) {
		t.Fatal("exact entry count bound rejected")
	}
	base := `[{"type":"thinking_dropped","path":"","reason":""}]`
	raw := strings.Replace(base, `"path":""`, `"path":"`+strings.Repeat("x", structuredInputTransformationBytes-len(base))+`"`, 1)
	if len(raw) != structuredInputTransformationBytes || !validInputTransformations(jsontext.Value(raw)) {
		t.Fatal("exact byte bound rejected")
	}
}

func TestStructuredAssistantInputTransformationsKeepExecutionAndSiblingIsolation(t *testing.T) {
	for _, name := range []string{"Bash", "Read", "Write", "Edit", "Glob", "Grep", "Agent", "Task", "Web", "WebFetch", "WebSearch", "mcp__foreign__read", "UnknownTool"} {
		t.Run(name, func(t *testing.T) {
			frame, message := transformationFixtureFrame(t)
			var content []map[string]any
			_ = json.Unmarshal(message["content"], &content)
			content = append(content, map[string]any{"type": "tool_use", "id": "private-side-effect-id", "name": name, "input": map[string]any{"command": "private-transform-command"}})
			message["content"], _ = json.Marshal(content)
			p, err := layerReadStream(t, metadataFixtureStream(t, frame, message))
			e := requireStructuredStage(t, err, "structured_execution_rejected")
			if e.StreamReason() != "stream_tool_execution" || p.Action != nil || p.Final != nil || strings.Contains(e.Error(), "private-transform-command") {
				t.Fatal("metadata authorized a side-effecting tool or leaked an input")
			}
		})
	}
	for _, typ := range []string{"tool_use", "server_tool_use", "mcp_tool_use", "tool_result", "control_request", "task_started", "action"} {
		t.Run("injected-"+typ, func(t *testing.T) {
			frame, message := transformationFixtureFrame(t)
			message["input_transformations"] = jsontext.Value(`[{"type":"` + typ + `","name":"Bash","input":{"command":"private-transform-command"}}]`)
			p, err := layerReadStream(t, metadataFixtureStream(t, frame, message))
			e := requireStructuredStage(t, err, "structured_serializer_frame_invalid")
			if e.StructuredDetails().AssistantReason != "assistant_input_transformations_invalid" || p.Action != nil || p.Final != nil {
				t.Fatal("injected execution object admitted")
			}
		})
	}
	for _, scope := range []string{"assistant", "message"} {
		t.Run("unknown-sibling-"+scope, func(t *testing.T) {
			frame, message := transformationFixtureFrame(t)
			if scope == "message" {
				message["future_metadata_field"] = jsontext.Value(`[]`)
			} else {
				frame["input_transformations"] = message["input_transformations"]
			}
			p, err := layerReadStream(t, metadataFixtureStream(t, frame, message))
			e := requireStructuredStage(t, err, "structured_serializer_frame_invalid")
			d := e.StructuredDetails()
			if d.AssistantReason != "assistant_unexpected_metadata" || d.UnexpectedMetadataScope != scope || p.Action != nil || p.Final != nil {
				t.Fatal("exception expanded to an unknown sibling or wrong scope")
			}
			if scope == "message" && (d.UnexpectedMetadataField != "future_metadata_field" ||
				!slices.Contains(d.MessageFields, StreamField{"input_transformations", "array"}) ||
				!slices.Equal(d.UnknownMessageFields, []StreamFieldCount{{JSONType: "array", Count: 1}})) {
				t.Fatal("known transformation/unknown sibling shape misreported")
			}
		})
	}
}

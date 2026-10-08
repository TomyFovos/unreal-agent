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

// unknown_fields in the original safe diagnostic is an aggregate label, not
// evidence of that literal JSON key. This negative fixture reproduces the
// observed shape without guessing either actual field name. Literal keys are
// deliberately unsupported until an installed producer contract declares them.
func TestStructuredAssistantUnknownMetadataReproducesObservedShape(t *testing.T) {
	data := layerCorpus(t, "unknown-metadata")
	for _, tc := range []struct{ name, wire, field string }{
		{"literal-label", string(data), "unknown_fields"},
		{"other-keys-same-shape", strings.ReplaceAll(string(data), `"unknown_fields"`, `"future_metadata_field"`), "future_metadata_field"},
		{"unsafe-keys-same-shape", strings.ReplaceAll(string(data), `"unknown_fields"`, `"private/unrecognized-key"`), "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := layerReadStream(t, []byte(tc.wire))
			e := requireStructuredStage(t, err, "structured_serializer_frame_invalid")
			d := e.StructuredDetails()
			literal := tc.name == "literal-label"
			if e.Code != "structured_protocol_invalid" || e.StreamReason() != "" ||
				d.UnexpectedMetadataScope != "assistant" || d.UnexpectedMetadataField != tc.field || d.AssistantUnknownFieldsPresent != literal || d.MessageUnknownFieldsPresent != literal ||
				d.AssistantReason != "assistant_unexpected_metadata" || d.Frame.Type != "assistant" || d.Frame.Subtype != "unknown" || d.Frame.SubtypePresent ||
				!slices.Equal(d.Frame.UnknownFields, []StreamFieldCount{{JSONType: "object", Count: 1}}) ||
				!slices.Equal(d.UnknownMessageFields, []StreamFieldCount{{JSONType: "array", Count: 1}}) ||
				!slices.Equal(d.ToolNameKinds, []string{"StructuredOutput"}) || d.ContentBlockCount != 1 || d.StructuredOutputBlocks != 1 ||
				d.PartialEventType != "content_block_delta" || d.DeltaType != "input_json_delta" || !d.SerializerInputPresent ||
				d.StructuredOutputPresent || d.SchemaChecked || p.Action != nil || p.Final != nil {
				t.Fatal("observed rejection boundary was not reproduced")
			}
			encoded, marshalErr := json.Marshal(d)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			for _, marker := range []string{"private-sensitive", "private-helper-id", "private-session-id", "private/unrecognized-key"} {
				if bytes.Contains(encoded, []byte(marker)) || strings.Contains(e.Error(), marker) {
					t.Fatal("unrecognized metadata leaked")
				}
			}
			if strings.Contains(e.Error(), "unknown_fields:object(1)") || !strings.Contains(e.Error(), "unrecognized_field_types:object(1)") {
				t.Fatal("aggregate summary is still ambiguous with a producer field")
			}
		})
	}
}

func metadataFixtureFrame(t *testing.T) (map[string]jsontext.Value, map[string]jsontext.Value) {
	t.Helper()
	lines := bytes.Split(layerCorpus(t, "unknown-metadata"), []byte("\n"))
	var frame, message map[string]jsontext.Value
	if json.Unmarshal(lines[3], &frame) != nil || json.Unmarshal(frame["message"], &message) != nil {
		t.Fatal("invalid synthetic fixture")
	}
	return frame, message
}

func metadataFixtureStream(t *testing.T, frame, message map[string]jsontext.Value) []byte {
	t.Helper()
	var err error
	frame["message"], err = json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	line, err := json.Marshal(frame, json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(layerCorpus(t, "unknown-metadata"), []byte("\n"))
	lines[3] = line
	return bytes.Join(lines, []byte("\n"))
}

func TestStructuredAssistantLiteralUnknownFieldsFailClosed(t *testing.T) {
	// Neither local pinned contract declares a literal unknown_fields key.
	// Do not add a success expectation for a guessed forward-compat container.
	for _, scope := range []string{"assistant", "message", "both"} {
		for _, tc := range []struct{ name, value string }{
			{"empty-object", `{}`}, {"empty-array", `[]`}, {"null", `null`},
			{"string", `"private-sensitive"`}, {"number", `42`}, {"boolean", `true`},
			{"opaque-object", `{"dummy":"private-sensitive"}`}, {"opaque-array", `["private-sensitive"]`},
			{"nested-execution", `{"nested":{"type":"tool_use","name":"Bash","input":{"command":"private-sensitive"}}}`},
			{"oversized", `{"dummy":"` + strings.Repeat("x", structuredResponseBytes+1) + `"}`},
		} {
			t.Run(scope+"/"+tc.name, func(t *testing.T) {
				frame, message := metadataFixtureFrame(t)
				delete(frame, "unknown_fields")
				delete(message, "unknown_fields")
				if scope != "message" {
					frame["unknown_fields"] = jsontext.Value(tc.value)
				}
				if scope != "assistant" {
					message["unknown_fields"] = jsontext.Value(tc.value)
				}
				p, err := layerReadStream(t, metadataFixtureStream(t, frame, message))
				if scope == "both" && tc.name == "oversized" {
					// Both containers exceed the existing JSONL frame bound before
					// any assistant fields may be inspected or retained.
					e := requireStructuredStage(t, err, "structured_response_oversized")
					if e.Code != "structured_response_too_large" || p.Action != nil || p.Final != nil || e.StructuredDetails().Frame != nil {
						t.Fatal("oversized frame crossed the parser resource boundary")
					}
					return
				}
				e := requireStructuredStage(t, err, "structured_serializer_frame_invalid")
				d := e.StructuredDetails()
				wantScope := "assistant"
				if scope == "message" {
					wantScope = "message"
				}
				if d.UnexpectedMetadataScope != wantScope || d.UnexpectedMetadataField != "unknown_fields" ||
					d.AssistantReason != "assistant_unexpected_metadata" || e.StreamReason() != "" || p.Action != nil || p.Final != nil {
					t.Fatal("unverified metadata became execution/proposal or lost its closed location")
				}
			})
		}
	}
}

func TestStructuredAssistantMetadataIsolationAndAuthoritativeResult(t *testing.T) {
	frame, message := metadataFixtureFrame(t)
	delete(frame, "unknown_fields")
	delete(message, "unknown_fields")
	p, err := layerReadStream(t, metadataFixtureStream(t, frame, message))
	if err != nil || p.Action != nil || p.Final == nil || p.Final.Message != "public completion" {
		t.Fatal("declared metadata/StructuredOutput partial falsely classified as execution", err)
	}
	for _, name := range []string{"Bash", "Read", "Write", "Edit", "Glob", "Grep", "Agent", "Task", "Web", "WebFetch", "WebSearch", "mcp__foreign__read", "UnknownTool"} {
		t.Run(name, func(t *testing.T) {
			wire := bytes.ReplaceAll(metadataFixtureStream(t, frame, message), []byte(`"name":"StructuredOutput"`), []byte(`"name":"`+name+`"`))
			p, err := layerReadStream(t, wire)
			e := requireStructuredStage(t, err, "structured_execution_rejected")
			if e.StreamReason() != "stream_tool_execution" || p.Action != nil || p.Final != nil {
				t.Fatal("actual unowned tool execution was not rejected")
			}
		})
	}
	wire := metadataFixtureStream(t, frame, message)
	result := bytes.Index(wire, []byte(`{"type":"result"`))
	if result < 0 {
		t.Fatal("result absent in fixture")
	}
	p, err = layerReadStream(t, wire[:result])
	requireStructuredStage(t, err, "structured_stream_incomplete")
	if p.Action != nil || p.Final != nil {
		t.Fatal("serializer input was used instead of final structured_output")
	}
}

func TestStructuredAssistantMessageMetadataNamesAreBoundedAndValueFree(t *testing.T) {
	for _, tc := range []struct{ label, name, value, reported string }{
		{"future-array", "future_metadata_field", `[]`, "future_metadata_field"},
		{"another-object", "another_field", `{}`, "another_field"},
		{"allowed-characters", "Future.metadata-9_0", `null`, "Future.metadata-9_0"},
		{"64-bytes", strings.Repeat("f", 64), `true`, strings.Repeat("f", 64)},
		{"65-bytes", strings.Repeat("f", 65), `[]`, "unknown"},
		{"empty-name", "", `{}`, "unknown"},
		{"non-ascii", "future_メタデータ", `[]`, "unknown"},
		{"unsafe-punctuation", "future:metadata", `{}`, "unknown"},
		{"slash", "future/metadata", `[]`, "unknown"},
		{"whitespace", "future metadata", `{}`, "unknown"},
		{"terminal-control", "future\x1bmetadata", `[]`, "unknown"},
		{"newline", "future\nmetadata", `{}`, "unknown"},
		{"string-value", "future_metadata_field", `"private-value-sentinel"`, "future_metadata_field"},
		{"nested-object-value", "another_field", `{"credential":"private-value-sentinel","session_id":"private-value-session-id","input":{"path":"private-value-path"},"assistant_text":"private-value-prose"}`, "another_field"},
		{"nested-array-value", "future_metadata_field", `["private-value-sentinel",{"type":"tool_use","name":"Bash","input":{"command":"private-value-command"}}]`, "future_metadata_field"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			frame, message := wireFixtureFrame(t)
			message[tc.name] = jsontext.Value(tc.value)
			p, err := layerReadStream(t, metadataFixtureStream(t, frame, message))
			e := requireStructuredStage(t, err, "structured_serializer_frame_invalid")
			d := e.StructuredDetails()
			if e.Code != "structured_protocol_invalid" || e.StreamReason() != "" ||
				d.AssistantReason != "assistant_unexpected_metadata" || d.UnexpectedMetadataScope != "message" ||
				d.UnexpectedMetadataField != tc.reported || d.SchemaChecked || p.Action != nil || p.Final != nil {
				t.Fatal("rejected message metadata lost its safe name or became a proposal")
			}
			if !strings.Contains(e.Error(), "metadata_scope=message metadata_field="+tc.reported+" ") ||
				!slices.Equal(d.UnknownMessageFields, []StreamFieldCount{{JSONType: fieldJSONType(jsontext.Value(tc.value)), Count: 1}}) {
				t.Fatal("safe rejection location/value type missing from diagnostics")
			}
			encoded, marshalErr := json.Marshal(d)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			for _, marker := range []string{"private-value-sentinel", "private-value-session-id", "private-value-path", "private-value-prose", "private-value-command", "private-wire-payload", "private-helper-id", "private-session-id"} {
				if bytes.Contains(encoded, []byte(marker)) || strings.Contains(e.Error(), marker) {
					t.Fatal("metadata value or private serializer data leaked")
				}
			}
			if tc.reported == "unknown" && tc.name != "" {
				key, _ := json.Marshal(tc.name)
				if bytes.Contains(encoded, key) || strings.Contains(e.Error(), tc.name) {
					t.Fatal("unsafe or oversized metadata key leaked")
				}
			}
		})
	}
}

func TestStructuredAssistantRejectedMetadataNamesAreScoped(t *testing.T) {
	for _, tc := range []struct{ scope, name, value, reported string }{
		{"assistant", "context_usage", `{}`, "context_usage"},
		{"assistant", "wire_ingest_context", `{}`, "wire_ingest_context"},
		{"assistant", "api_error_params", `{}`, "api_error_params"},
		{"assistant", "local_command_run", `{}`, "local_command_run"},
		{"assistant", "private/sensitive", `{}`, "unknown"},
		{"message", "private/sensitive", `["private-sensitive"]`, "unknown"},
	} {
		t.Run(tc.scope+"/"+tc.name, func(t *testing.T) {
			frame, message := metadataFixtureFrame(t)
			delete(frame, "unknown_fields")
			delete(message, "unknown_fields")
			if tc.scope == "assistant" {
				frame[tc.name] = jsontext.Value(tc.value)
			} else {
				message[tc.name] = jsontext.Value(tc.value)
			}
			_, err := layerReadStream(t, metadataFixtureStream(t, frame, message))
			e := requireStructuredStage(t, err, "structured_serializer_frame_invalid")
			d := e.StructuredDetails()
			if d.UnexpectedMetadataScope != tc.scope || d.UnexpectedMetadataField != tc.reported || strings.Contains(e.Error(), "private-sensitive") {
				t.Fatal("unsafe or ambiguous rejected metadata name")
			}
		})
	}
}

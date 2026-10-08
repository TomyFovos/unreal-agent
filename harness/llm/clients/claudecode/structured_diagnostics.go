package claudecode

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
)

// These are branch identifiers, never provider-supplied strings. Diagnostics
// retain neither wire bytes nor decoder/schema errors (which contain values).
type structuredStage uint8

const (
	structuredInitFrameInvalid structuredStage = iota + 1
	structuredUnexpectedPreinitFrame
	structuredInitIncomplete
	structuredSerializerFrameInvalid
	structuredSerializerPartialInvalid
	structuredResultInvalid
	structuredResultMissingOutput
	structuredResultNullOutput
	structuredJSONDecodeFailed
	structuredDuplicateFields
	structuredTrailingJSON
	structuredSchemaRejected
	structuredArgumentsSchemaRejected
	structuredUnknownTool
	structuredUnexpectedControlFrame
	structuredTrailingFrame
	structuredStreamIncomplete
	structuredActionEnvelopeInvalid
	structuredResponseOversized
	structuredExecutionRejected
	structuredSystemFrameInvalid
	structuredStreamFrameInvalid
	structuredPermissionDenied
	structuredUsageInvalid
	structuredSerializerRoundLimit
)

func (e *Error) StructuredStage() string {
	switch e.structuredStage {
	case structuredInitFrameInvalid:
		return "structured_init_frame_invalid"
	case structuredUnexpectedPreinitFrame:
		return "structured_unexpected_preinit_frame"
	case structuredInitIncomplete:
		return "structured_init_incomplete"
	case structuredSerializerFrameInvalid:
		return "structured_serializer_frame_invalid"
	case structuredSerializerPartialInvalid:
		return "structured_serializer_partial_invalid"
	case structuredResultInvalid:
		return "structured_result_invalid"
	case structuredResultMissingOutput:
		return "structured_result_missing_output"
	case structuredResultNullOutput:
		return "structured_result_null_output"
	case structuredJSONDecodeFailed:
		return "structured_json_decode_failed"
	case structuredDuplicateFields:
		return "structured_duplicate_fields"
	case structuredTrailingJSON:
		return "structured_trailing_json"
	case structuredSchemaRejected:
		return "structured_schema_rejected"
	case structuredArgumentsSchemaRejected:
		return "structured_arguments_schema_rejected"
	case structuredUnknownTool:
		return "structured_unknown_tool"
	case structuredUnexpectedControlFrame:
		return "structured_unexpected_control_frame"
	case structuredTrailingFrame:
		return "structured_trailing_frame"
	case structuredStreamIncomplete:
		return "structured_stream_incomplete"
	case structuredActionEnvelopeInvalid:
		return "structured_action_envelope_invalid"
	case structuredResponseOversized:
		return "structured_response_oversized"
	case structuredExecutionRejected:
		return "structured_execution_rejected"
	case structuredSystemFrameInvalid:
		return "structured_system_frame_invalid"
	case structuredStreamFrameInvalid:
		return "structured_stream_frame_invalid"
	case structuredPermissionDenied:
		return "structured_permission_denied"
	case structuredUsageInvalid:
		return "structured_usage_invalid"
	case structuredSerializerRoundLimit:
		return "structured_serializer_round_limit"
	default:
		return ""
	}
}

// StructuredDiagnostics is a safe, immutable error projection. Discriminants
// and shape fields use a closed vocabulary. One unknown assistant/message member
// name may additionally pass the bounded ASCII rule below; no values survive.
type StructuredDiagnostics struct {
	Stage                         string
	Frame                         *StreamShape
	AssistantReason               string
	UnexpectedMetadataScope       string
	UnexpectedMetadataField       string
	AssistantUnknownFieldsPresent bool
	MessageUnknownFieldsPresent   bool
	MessageFields                 []StreamField
	UnknownMessageFields          []StreamFieldCount
	ContentBlockCount             int
	ToolNameKinds                 []string
	ContentBlockTypes             []string
	PartialEventType              string
	DeltaType                     string
	StructuredOutputBlocks        int
	SerializerInputPresent        bool
	StructuredOutputPresent       bool
	StructuredOutputNull          bool
	SchemaChecked                 bool
	SchemaValid                   bool
	FrameBytes                    int
	SerializerInputBytes          int
	StructuredOutputBytes         int
}

func (e *Error) StructuredDetails() *StructuredDiagnostics {
	if e.structuredDetail == nil {
		return nil
	}
	d := *e.structuredDetail
	d.Stage = e.StructuredStage()
	d.ContentBlockTypes = slices.Clone(d.ContentBlockTypes)
	d.MessageFields = slices.Clone(d.MessageFields)
	d.UnknownMessageFields = slices.Clone(d.UnknownMessageFields)
	d.ToolNameKinds = slices.Clone(d.ToolNameKinds)
	if d.Frame != nil {
		frame := *d.Frame
		frame.Fields = slices.Clone(frame.Fields)
		frame.UnknownFields = slices.Clone(frame.UnknownFields)
		d.Frame = &frame
	}
	return &d
}

func (d StructuredDiagnostics) summary() string {
	fields := ""
	if d.AssistantReason != "" {
		fields = "reason=" + d.AssistantReason + " "
	}
	if d.UnexpectedMetadataScope != "" {
		fields += "metadata_scope=" + d.UnexpectedMetadataScope + " metadata_field=" + d.UnexpectedMetadataField + " "
	}
	if d.Frame != nil {
		// The old label was mistaken for an actual producer field. These are
		// counts of arbitrary, unrecognized keys, grouped by JSON value type.
		fields += strings.ReplaceAll(d.Frame.summary(), "unknown_fields:", "unrecognized_field_types:") + " "
	}
	if len(d.MessageFields) != 0 || len(d.UnknownMessageFields) != 0 {
		var names []string
		for _, f := range d.MessageFields {
			names = append(names, f.Name+":"+f.JSONType)
		}
		for _, f := range d.UnknownMessageFields {
			names = append(names, "unrecognized_field_types:"+f.JSONType+"("+strconv.Itoa(f.Count)+")")
		}
		fields += "message_fields=[" + strings.Join(names, ",") + "] "
	}
	return fields + "assistant_literal_unknown_fields_present=" + strconv.FormatBool(d.AssistantUnknownFieldsPresent) +
		" message_literal_unknown_fields_present=" + strconv.FormatBool(d.MessageUnknownFieldsPresent) +
		" blocks=[" + strings.Join(d.ContentBlockTypes, ",") + "] serializer_count=" + strconv.Itoa(d.StructuredOutputBlocks) +
		" content_block_count=" + strconv.Itoa(d.ContentBlockCount) + " tool_kinds=[" + strings.Join(d.ToolNameKinds, ",") + "]" +
		" partial_event=" + d.PartialEventType + " delta_type=" + d.DeltaType +
		" serializer_input_present=" + strconv.FormatBool(d.SerializerInputPresent) +
		" structured_output_present=" + strconv.FormatBool(d.StructuredOutputPresent) +
		" structured_output_null=" + strconv.FormatBool(d.StructuredOutputNull) +
		" schema_checked=" + strconv.FormatBool(d.SchemaChecked) + " schema_valid=" + strconv.FormatBool(d.SchemaValid) +
		" frame_bytes=" + strconv.Itoa(d.FrameBytes) + " serializer_input_bytes=" + strconv.Itoa(d.SerializerInputBytes) +
		" structured_output_bytes=" + strconv.Itoa(d.StructuredOutputBytes)
}

func structuredError(code string, stage structuredStage) *Error {
	return &Error{Code: code, structuredStage: stage}
}

// The original typed category and a more specific stage from schema parsing
// survive annotation. Only safe metadata is copied, never the underlying error.
func annotateStructured(err error, stage structuredStage, line []byte, state StructuredDiagnostics) error {
	var e *Error
	if !errors.As(err, &e) {
		e = &Error{Code: "structured_protocol_invalid"}
	}
	copy := *e
	if copy.structuredStage == 0 {
		copy.structuredStage = stage
	}
	if line != nil {
		state.Frame = structuredShape(line)
		state.FrameBytes = len(line)
		if state.Frame.Type == "assistant" {
			noteStructuredAssistantShape(&state, line)
		}
	}
	if e.structuredDetail != nil {
		state.StructuredOutputPresent = e.structuredDetail.StructuredOutputPresent
		state.StructuredOutputNull = e.structuredDetail.StructuredOutputNull
		state.StructuredOutputBytes = e.structuredDetail.StructuredOutputBytes
		state.SchemaChecked = e.structuredDetail.SchemaChecked
		state.SchemaValid = e.structuredDetail.SchemaValid
	}
	state.ContentBlockTypes = slices.Clone(state.ContentBlockTypes)
	state.MessageFields = slices.Clone(state.MessageFields)
	state.UnknownMessageFields = slices.Clone(state.UnknownMessageFields)
	state.ToolNameKinds = slices.Clone(state.ToolNameKinds)
	copy.structuredDetail = &state
	return &copy
}

func structuredShape(line []byte) *StreamShape {
	s := systemShape(line)
	var fields map[string]jsontext.Value
	if json.Unmarshal(line, &fields) != nil || fields == nil {
		return s
	}
	var typ, subtype string
	_ = json.Unmarshal(fields["type"], &typ)
	_ = json.Unmarshal(fields["subtype"], &subtype)
	switch typ {
	case "system", "assistant", "user", "stream_event", "result", "control_request", "control_response", "control_cancel_request", "keep_alive", "tool_progress", "tool_use_summary", "error", "rate_limit_event":
		s.Type = typ
	}
	if typ == "result" {
		switch subtype {
		case "success", "error_during_execution", "error_max_turns", "error_max_budget_usd", "error_max_structured_output_retries":
			s.Subtype = subtype
		}
	}
	// Extend the diagnostic vocabulary only; this is not an acceptance list.
	for _, name := range []string{"structured_output", "response", "tool_name", "elapsed_time_seconds", "preceding_tool_use_ids", "timestamp", "user_message_uuids", "resumed_from_incomplete_thinking", "supersedes", "aborted", "historical", "resume_reason", "tool_use_meta", "narration_block_indexes", "wire_tool_inputs", "isSynthetic"} {
		if raw, exists := fields[name]; exists && !publicSystemField(name) {
			s.Fields = append(s.Fields, StreamField{Name: name, JSONType: fieldJSONType(raw)})
			for i := range s.UnknownFields {
				if s.UnknownFields[i].JSONType == fieldJSONType(raw) {
					s.UnknownFields[i].Count--
				}
			}
		}
	}
	s.UnknownFields = slices.DeleteFunc(s.UnknownFields, func(c StreamFieldCount) bool { return c.Count == 0 })
	slices.SortFunc(s.Fields, func(a, b StreamField) int { return strings.Compare(a.Name, b.Name) })
	return s
}

// Shape field names come from the pinned public Message/installed wire schemas.
// Unknown keys stay grouped by value type here. The separate metadata location
// may expose one member name under its bounded ASCII diagnostic rule. A provider
// error can terminate before the parser visits an unknown key: record its name
// for observation without changing the error category or accepting that key.
func noteStructuredAssistantShape(d *StructuredDiagnostics, line []byte) {
	var frame struct {
		Message       map[string]jsontext.Value `json:"message"`
		UnknownFields jsontext.Value            `json:"unknown_fields"`
	}
	if json.Unmarshal(line, &frame) != nil || frame.Message == nil {
		return
	}
	d.AssistantUnknownFieldsPresent = len(frame.UnknownFields) != 0
	_, d.MessageUnknownFieldsPresent = frame.Message["unknown_fields"]
	if d.UnexpectedMetadataScope == "" && d.Frame != nil && len(d.Frame.UnknownFields) != 0 {
		var fields map[string]jsontext.Value
		if json.Unmarshal(line, &fields) == nil {
			known := map[string]bool{}
			for _, field := range d.Frame.Fields {
				known[field.Name] = true
			}
			var names []string
			for name := range fields {
				if !known[name] {
					names = append(names, name)
				}
			}
			slices.Sort(names)
			if len(names) != 0 {
				d.UnexpectedMetadataScope = "assistant"
				d.UnexpectedMetadataField = structuredMetadataFieldName("assistant", names[0])
			}
		}
	}
	d.MessageFields, d.UnknownMessageFields = nil, nil
	unknown := map[string]int{}
	for name, raw := range frame.Message {
		switch name {
		case "id", "type", "role", "model", "content", "stop_reason", "stop_sequence", "stop_details", "usage", "container", "context_management", "diagnostics", "input_transformations":
			d.MessageFields = append(d.MessageFields, StreamField{Name: name, JSONType: fieldJSONType(raw)})
		default:
			unknown[fieldJSONType(raw)]++
		}
	}
	for typ, count := range unknown {
		d.UnknownMessageFields = append(d.UnknownMessageFields, StreamFieldCount{JSONType: typ, Count: count})
	}
	slices.SortFunc(d.MessageFields, func(a, b StreamField) int { return strings.Compare(a.Name, b.Name) })
	slices.SortFunc(d.UnknownMessageFields, func(a, b StreamFieldCount) int { return strings.Compare(a.JSONType, b.JSONType) })
	var blocks []map[string]jsontext.Value
	if json.Unmarshal(frame.Message["content"], &blocks) != nil {
		return
	}
	d.ContentBlockCount = len(blocks)
	for _, b := range blocks {
		var typ string
		_ = json.Unmarshal(b["type"], &typ)
		d.ContentBlockTypes = appendUnique(d.ContentBlockTypes, publicStructuredBlock(typ))
		if typ == "tool_use" {
			d.ToolNameKinds = appendUnique(d.ToolNameKinds, structuredToolKind(b["name"]))
			if jsonStringEquals(b["name"], structuredOutputTool) {
				d.SerializerInputPresent = len(b["input"]) != 0
				d.SerializerInputBytes = len(b["input"])
			}
		}
	}
}

// This is diagnostic-only, NEVER an acceptance list. An assistant/message key
// is shown exactly only when it is 1..64 ASCII bytes from [A-Za-z0-9_.-]. Its
// value is never passed here. The scope itself remains a closed vocabulary.
func structuredMetadataFieldName(scope, name string) string {
	if scope == "message" || scope == "assistant" {
		if len(name) == 0 || len(name) > 64 {
			return "unknown"
		}
		for i := range len(name) {
			c := name[i]
			switch {
			case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_', c == '.', c == '-':
			default:
				return "unknown"
			}
		}
		// Keep only the bounded key, without retaining a backing wire buffer.
		return strings.Clone(name)
	}
	return "unknown"
}

func publicStructuredBlock(typ string) string {
	switch typ {
	case "text", "thinking", "redacted_thinking", "tool_use", "tool_result", "server_tool_use", "mcp_tool_use", "mcp_tool_result":
		return typ
	default:
		return "unknown"
	}
}

func noteStructuredBlocks(d *StructuredDiagnostics, record streamRecord) {
	if record.Type == "stream_event" {
		d.PartialEventType, d.DeltaType = "unknown", "unknown"
		switch record.Event.Type {
		case "message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop", "ping":
			d.PartialEventType = record.Event.Type
		}
		switch record.Event.Delta.Type {
		case "text_delta", "thinking_delta", "signature_delta", "input_json_delta":
			d.DeltaType = record.Event.Delta.Type
		}
	}
	blocks := append([]block{record.Event.ContentBlock}, record.Event.Message.Content...)
	blocks = append(blocks, record.Message.Content...)
	for _, b := range blocks {
		if b.Type == "" {
			continue
		}
		typ := publicStructuredBlock(b.Type)
		if !slices.Contains(d.ContentBlockTypes, typ) {
			d.ContentBlockTypes = append(d.ContentBlockTypes, typ)
			slices.Sort(d.ContentBlockTypes)
		}
		if b.Type == "tool_use" && b.Name == structuredOutputTool && len(b.Input) != 0 {
			d.SerializerInputPresent = true
			d.SerializerInputBytes = len(b.Input)
		}
	}
}

// Read exactly one JSON value before semantic decoding. This separates
// duplicate keys and trailing data without retaining paths/values from errors.
func validateStructuredJSON(raw []byte) error {
	decoder := jsontext.NewDecoder(bytes.NewReader(raw))
	if _, err := decoder.ReadValue(); err != nil {
		stage := structuredJSONDecodeFailed
		if errors.Is(err, jsontext.ErrDuplicateName) {
			stage = structuredDuplicateFields
		}
		return structuredError("structured_protocol_invalid", stage)
	}
	if _, err := decoder.ReadValue(); err != io.EOF {
		return structuredError("structured_protocol_invalid", structuredTrailingJSON)
	}
	return nil
}

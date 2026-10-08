package claudecode

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"maps"
	"slices"
)

// SDKAssistantMessage is discriminated by type, not subtype. The installed
// 2.1.285 BetaMessage wire schema has an optional id and an opaque string model.
// Neither is authorization nor a canonical message identity. Validate content
// before removing private helper metadata; only the terminal structured_output
// can become an Unreal Action.
func structuredAssistant(line []byte, diagnostic *StructuredDiagnostics) (streamRecord, error) {
	invalid := func(reason string) (streamRecord, error) {
		diagnostic.AssistantReason = reason // callers use only closed literals below
		return streamRecord{}, structuredError("structured_protocol_invalid", structuredSerializerFrameInvalid)
	}
	execution := func(reason string) (streamRecord, error) {
		diagnostic.AssistantReason = reason
		err := toolStreamFailure()
		err.structuredStage = structuredExecutionRejected
		return streamRecord{}, err
	}
	unrecognized := func(scope, name string) (streamRecord, error) {
		// An unknown metadata key is a protocol rejection, not evidence that
		// a side-effecting tool ran. Only diagnostic-safe key names survive;
		// a rejected message name passes the bounded ASCII rule, never a value.
		diagnostic.UnexpectedMetadataScope = scope
		diagnostic.UnexpectedMetadataField = structuredMetadataFieldName(scope, name)
		return invalid("assistant_unexpected_metadata")
	}
	var fields map[string]jsontext.Value
	if json.Unmarshal(line, &fields) != nil || fields == nil {
		return invalid("assistant_envelope_invalid")
	}
	if _, exists := fields["subtype"]; exists {
		// The pinned assistant schema declares no subtype. Absence is normal;
		// do not import the system/init/status discriminator vocabulary here.
		return invalid("assistant_subtype_unsupported")
	}
	var wireInputs map[string]jsontext.Value
	for _, key := range slices.Sorted(maps.Keys(fields)) {
		raw := fields[key]
		switch key {
		case "type", "message":
		case "parent_tool_use_id", "subagent_type", "task_description":
			if raw.Kind() != 'n' && !jsonStringEquals(raw, "") {
				return execution("assistant_execution_metadata")
			}
		case "uuid", "session_id", "request_id", "timestamp", "user_message_uuid", "resume_reason":
			if raw.Kind() != '"' {
				return invalid("assistant_metadata_invalid")
			}
		case "user_message_uuids", "supersedes":
			var ids []string
			if raw.Kind() != '[' || json.Unmarshal(raw, &ids) != nil {
				return invalid("assistant_metadata_invalid")
			}
		case "resumed_from_incomplete_thinking", "historical", "aborted":
			if raw.Kind() != 't' && raw.Kind() != 'f' {
				return invalid("assistant_metadata_invalid")
			}
			if key == "aborted" && raw.Kind() == 't' {
				diagnostic.AssistantReason = "assistant_aborted"
				return streamRecord{}, structuredError("structured_protocol_invalid", structuredStreamIncomplete)
			}
		case "error":
			if raw.Kind() != 'n' {
				if raw.Kind() != '"' {
					return invalid("assistant_error_invalid")
				}
				err := assistantFailure(raw)
				diagnostic.AssistantReason = err.AssistantReason()
				err.structuredStage = structuredResultInvalid
				return streamRecord{}, err
			}
		case "tool_use_meta", "narration_block_indexes": // installed CLI render metadata; checked below
		case "wire_tool_inputs": // installed CLI raw-input replay metadata, never authoritative
			if len(raw) > structuredWireInputBytes {
				diagnostic.AssistantReason = "assistant_wire_tool_inputs_oversized"
				diagnostic.UnexpectedMetadataScope = "assistant"
				diagnostic.UnexpectedMetadataField = "wire_tool_inputs"
				return streamRecord{}, structuredError("structured_response_too_large", structuredResponseOversized)
			}
			var valid bool
			wireInputs, valid = serializerWireInputs(raw)
			if !valid {
				diagnostic.UnexpectedMetadataScope = "assistant"
				diagnostic.UnexpectedMetadataField = "wire_tool_inputs"
				return invalid("assistant_wire_tool_inputs_invalid")
			}
		default:
			// Do not silently erase unknown execution/continuation fields while
			// converting a helper frame to the text-only parser's representation.
			return unrecognized("assistant", key)
		}
	}
	var message map[string]jsontext.Value
	if fields["message"].Kind() != '{' || json.Unmarshal(fields["message"], &message) != nil {
		return invalid("assistant_message_invalid")
	}
	for _, key := range slices.Sorted(maps.Keys(message)) {
		raw := message[key]
		switch key {
		case "id":
			if raw.Kind() != '"' || jsonStringEquals(raw, "") {
				return invalid("assistant_id_invalid")
			}
		case "model", "content": // checked below
		case "type":
			if !jsonStringEquals(raw, "message") {
				return invalid("assistant_message_type_invalid")
			}
		case "role":
			if !jsonStringEquals(raw, "assistant") {
				return invalid("assistant_role_invalid")
			}
		case "stop_reason":
			if raw.Kind() != 'n' {
				var reason string
				if json.Unmarshal(raw, &reason) != nil || !slices.Contains([]string{"", "end_turn", "max_tokens", "stop_sequence", "tool_use", "pause_turn", "refusal"}, reason) {
					return invalid("assistant_stop_invalid")
				}
			}
		case "stop_sequence":
			if raw.Kind() != 'n' && raw.Kind() != '"' {
				return invalid("assistant_stop_invalid")
			}
		case "input_transformations": // Messages API input metadata, never an execution proposal
			if len(raw) > structuredInputTransformationBytes {
				diagnostic.AssistantReason = "assistant_input_transformations_oversized"
				diagnostic.UnexpectedMetadataScope = "message"
				diagnostic.UnexpectedMetadataField = "input_transformations"
				return streamRecord{}, structuredError("structured_response_too_large", structuredResponseOversized)
			}
			if !validInputTransformations(raw) {
				diagnostic.UnexpectedMetadataScope = "message"
				diagnostic.UnexpectedMetadataField = "input_transformations"
				return invalid("assistant_input_transformations_invalid")
			}
		case "container":
			if raw.Kind() != 'n' {
				return execution("assistant_execution_metadata")
			}
		case "usage":
			if raw.Kind() != '{' {
				return invalid("assistant_usage_invalid")
			}
			var usage struct {
				Server jsontext.Value `json:"server_tool_use"`
			}
			if json.Unmarshal(raw, &usage) != nil {
				return invalid("assistant_usage_invalid")
			}
			if len(usage.Server) != 0 && usage.Server.Kind() != 'n' {
				var counts map[string]int64
				if usage.Server.Kind() != '{' || json.Unmarshal(usage.Server, &counts) != nil {
					return execution("assistant_execution_metadata")
				}
				for _, key := range slices.Sorted(maps.Keys(counts)) {
					count := counts[key]
					if (key != "web_search_requests" && key != "web_fetch_requests") || count != 0 {
						return execution("assistant_execution_metadata")
					}
				}
			}
		case "stop_details", "context_management", "diagnostics": // private API metadata, never replayed
			if raw.Kind() != 'n' && raw.Kind() != '{' {
				return invalid("assistant_metadata_invalid")
			}
		default:
			return unrecognized("message", key)
		}
	}
	if message["model"].Kind() != '"' || jsonStringEquals(message["model"], "") {
		return invalid("assistant_model_invalid")
	}
	var content []map[string]jsontext.Value
	if message["content"].Kind() != '[' || json.Unmarshal(message["content"], &content) != nil {
		return invalid("assistant_content_invalid")
	}
	serializerIDs := map[string]bool{}
	for _, b := range content {
		var typ string
		if json.Unmarshal(b["type"], &typ) != nil || b["type"].Kind() != '"' {
			return invalid("assistant_block_invalid")
		}
		diagnostic.ContentBlockTypes = appendUnique(diagnostic.ContentBlockTypes, publicStructuredBlock(typ))
		var names []string
		switch typ {
		case "tool_use":
			diagnostic.ToolNameKinds = appendUnique(diagnostic.ToolNameKinds, structuredToolKind(b["name"]))
			if !jsonStringEquals(b["name"], structuredOutputTool) {
				return execution("assistant_tool_rejected")
			}
			if b["id"].Kind() != '"' || jsonStringEquals(b["id"], "") {
				return invalid("assistant_serializer_id_invalid")
			}
			diagnostic.SerializerInputPresent = len(b["input"]) != 0
			diagnostic.SerializerInputBytes = len(b["input"])
			if b["input"].Kind() != '{' {
				return invalid("assistant_serializer_input_invalid")
			}
			if raw, exists := b["caller"]; exists && !isDirectSerializerCaller(raw) {
				return execution("assistant_execution_metadata")
			}
			var id string
			_ = json.Unmarshal(b["id"], &id)
			if serializerIDs[id] {
				return invalid("assistant_serializer_duplicate")
			}
			serializerIDs[id] = true
			names = []string{"type", "id", "name", "input", "caller"}
		case "text":
			if b["text"].Kind() != '"' {
				return invalid("assistant_block_invalid")
			}
			if raw, exists := b["citations"]; exists && raw.Kind() != '[' && raw.Kind() != 'n' {
				return invalid("assistant_block_invalid")
			}
			names = []string{"type", "text", "citations"}
		case "thinking":
			if b["thinking"].Kind() != '"' {
				return invalid("assistant_block_invalid")
			}
			if raw, exists := b["signature"]; exists && raw.Kind() != '"' {
				return invalid("assistant_block_invalid")
			}
			names = []string{"type", "thinking", "signature"}
		case "redacted_thinking":
			if b["data"].Kind() != '"' {
				return invalid("assistant_block_invalid")
			}
			names = []string{"type", "data"}
		default:
			return execution("assistant_block_rejected")
		}
		for _, name := range slices.Sorted(maps.Keys(b)) {
			if !slices.Contains(names, name) {
				return unrecognized("content_block", name)
			}
		}
	}
	if raw, exists := fields["tool_use_meta"]; exists && !isSerializerDisplayMetadata(raw, serializerIDs) {
		return execution("assistant_execution_metadata")
	}
	for id := range wireInputs {
		// The installed producer filters inputs to tool_use blocks in this
		// message (plus batched side-effect tools, which this adapter rejects).
		// Admit only metadata correlated to its checked StructuredOutput ids.
		if !serializerIDs[id] {
			diagnostic.UnexpectedMetadataScope = "assistant"
			diagnostic.UnexpectedMetadataField = "wire_tool_inputs"
			return invalid("assistant_wire_tool_inputs_invalid")
		}
	}
	if raw, exists := fields["narration_block_indexes"]; exists {
		var indexes []int
		if raw.Kind() != '[' || json.Unmarshal(raw, &indexes) != nil {
			return invalid("assistant_metadata_invalid")
		}
		for _, index := range indexes {
			if index < 0 || index >= len(content) || !jsonStringEquals(content[index]["type"], "text") {
				return invalid("assistant_metadata_invalid")
			}
		}
	}
	var record streamRecord
	if json.Unmarshal(line, &record) != nil {
		return invalid("assistant_decode_invalid")
	}
	return record, nil
}

const (
	structuredWireInputBytes   = structuredResponseBytes
	structuredWireInputEntries = 64
	structuredWireInputDepth   = 32
	structuredWireInputValues  = 4096
)

// Installed 2.1.285 declares wire_tool_inputs as an optional record of raw API
// inputs keyed by tool_use id. Vee/kRt capture/filter object inputs; Mo emits
// this wrapper field separately from the normalized message.content. The public
// pinned SDKAssistantMessage omits this @internal field. It is not an Action,
// an execution receipt or replay authority for Unreal. Do not compare it with
// the normalized helper input, schema-validate it as a proposal, or retain it.
func serializerWireInputs(raw jsontext.Value) (map[string]jsontext.Value, bool) {
	var inputs map[string]jsontext.Value
	if raw.Kind() != '{' || len(raw) > structuredWireInputBytes || json.Unmarshal(raw, &inputs) != nil || len(inputs) > structuredWireInputEntries {
		return nil, false
	}
	values := 0
	var inspect func(jsontext.Value, int) bool
	inspect = func(value jsontext.Value, depth int) bool {
		values++
		if depth > structuredWireInputDepth || values > structuredWireInputValues {
			return false
		}
		switch value.Kind() {
		case '{':
			var object map[string]jsontext.Value
			if json.Unmarshal(value, &object) != nil || serializerWireExecutionShape(object["type"]) {
				return false
			}
			for _, nested := range object {
				if !inspect(nested, depth+1) {
					return false
				}
			}
		case '[':
			var array []jsontext.Value
			if json.Unmarshal(value, &array) != nil {
				return false
			}
			for _, nested := range array {
				if !inspect(nested, depth+1) {
					return false
				}
			}
		}
		return true
	}
	for _, value := range inputs {
		if value.Kind() != '{' || !inspect(value, 1) {
			return nil, false
		}
	}
	return inputs, true
}

// Reject execution frames smuggled into this transport-only field. Action/Final
// envelopes and ordinary strings are opaque data here, never executable.
func serializerWireExecutionShape(raw jsontext.Value) bool {
	var typ string
	if raw.Kind() != '"' || json.Unmarshal(raw, &typ) != nil {
		return false
	}
	switch typ {
	case "tool_use", "server_tool_use", "mcp_tool_use", "mcp_tool_result", "tool_result", "tool_progress", "tool_use_summary",
		"control_request", "control_response", "task_started", "task_progress", "task_notification", "task_updated":
		return true
	}
	return false
}

func jsonStringEquals(raw jsontext.Value, want string) bool {
	var got string
	return raw.Kind() == '"' && json.Unmarshal(raw, &got) == nil && got == want
}

func isDirectSerializerCaller(raw jsontext.Value) bool {
	var fields map[string]jsontext.Value
	return raw.Kind() == '{' && json.Unmarshal(raw, &fields) == nil && len(fields) == 1 && jsonStringEquals(fields["type"], "direct")
}

func isSerializerDisplayMetadata(raw jsontext.Value, ids map[string]bool) bool {
	var metadata []map[string]jsontext.Value
	if raw.Kind() != '[' || json.Unmarshal(raw, &metadata) != nil {
		return false
	}
	for _, m := range metadata {
		var id string
		if len(m) != 2 || json.Unmarshal(m["id"], &id) != nil || !ids[id] || m["display_name"].Kind() != '"' {
			return false
		}
	}
	return true
}

func appendUnique(values []string, value string) []string {
	if !slices.Contains(values, value) {
		values = append(values, value)
		slices.Sort(values)
	}
	return values
}

func structuredToolKind(raw jsontext.Value) string {
	var name string
	if json.Unmarshal(raw, &name) != nil {
		return "unknown"
	}
	switch name {
	case structuredOutputTool:
		return structuredOutputTool
	case "Bash", "Read", "Write", "Edit", "Glob", "Grep", "Agent", "Task", "Web", "WebFetch", "WebSearch":
		return "side_effecting_builtin"
	}
	if len(name) >= 5 && name[:5] == "mcp__" {
		return "foreign_mcp"
	}
	return "unknown"
}

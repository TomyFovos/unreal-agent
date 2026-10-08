package claudecode

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"slices"
	"strconv"
	"strings"
)

// StreamShape contains schema information only. Names and discriminants are
// selected from the public SDK schema; arbitrary JSON keys and enum values can
// themselves contain secrets, so unknown ones are represented by type/count.
// No raw record, field value, decoder error, or nested body is retained.
type StreamShape struct {
	Type, Subtype                       string
	Object, TypePresent, SubtypePresent bool
	Fields                              []StreamField
	UnknownFields                       []StreamFieldCount
	OmittedFields                       int
}

type StreamField struct{ Name, JSONType string }
type StreamFieldCount struct {
	JSONType string
	Count    int
}

func (e *Error) SystemShape() *StreamShape {
	if e.systemShape == nil {
		return nil
	}
	s := *e.systemShape
	s.Fields = slices.Clone(s.Fields)
	s.UnknownFields = slices.Clone(s.UnknownFields)
	return &s
}

func unknownSystemShape(line []byte) *Error {
	e := malformedStream(streamSystemUnknownShape)
	e.systemShape = systemShape(line)
	return e
}

func systemShape(line []byte) *StreamShape {
	s := &StreamShape{Type: "unknown", Subtype: "unknown"}
	var fields map[string]jsontext.Value
	if json.Unmarshal(line, &fields) != nil || fields == nil {
		return s
	}
	s.Object = true
	_, s.TypePresent = fields["type"]
	_, s.SubtypePresent = fields["subtype"]
	var typ, subtype string
	_ = json.Unmarshal(fields["type"], &typ)
	_ = json.Unmarshal(fields["subtype"], &subtype)
	if typ == "system" {
		s.Type = "system"
	}
	s.Subtype = publicSystemSubtype(subtype)
	counts := map[string]int{}
	for name, raw := range fields {
		kind := fieldJSONType(raw)
		if publicSystemField(name) {
			s.Fields = append(s.Fields, StreamField{Name: name, JSONType: kind})
		} else {
			counts[kind]++
		}
	}
	slices.SortFunc(s.Fields, func(a, b StreamField) int { return strings.Compare(a.Name, b.Name) })
	if len(s.Fields) > 32 {
		s.OmittedFields = len(s.Fields) - 32
		s.Fields = s.Fields[:32]
	}
	for _, kind := range []string{"string", "number", "boolean", "null", "array", "object", "unknown"} {
		if n := counts[kind]; n > 0 {
			s.UnknownFields = append(s.UnknownFields, StreamFieldCount{JSONType: kind, Count: n})
		}
	}
	return s
}

func (s StreamShape) summary() string {
	var fields []string
	for _, field := range s.Fields {
		fields = append(fields, field.Name+":"+field.JSONType)
	}
	for _, field := range s.UnknownFields {
		fields = append(fields, "unknown_fields:"+field.JSONType+"("+strconv.Itoa(field.Count)+")")
	}
	if s.OmittedFields > 0 {
		fields = append(fields, "omitted_fields("+strconv.Itoa(s.OmittedFields)+")")
	}
	return "type=" + s.Type + " subtype=" + s.Subtype + " fields=[" + strings.Join(fields, ",") + "] object=" + strconv.FormatBool(s.Object) + " type_present=" + strconv.FormatBool(s.TypePresent) + " subtype_present=" + strconv.FormatBool(s.SubtypePresent)
}

func fieldJSONType(raw jsontext.Value) string {
	switch raw.Kind() {
	case '"':
		return "string"
	case '0':
		return "number"
	case 't', 'f':
		return "boolean"
	case 'n':
		return "null"
	case '[':
		return "array"
	case '{':
		return "object"
	default:
		return "unknown"
	}
}

// This is a diagnostic enum, not an acceptance list. Execution/context/control
// messages still fail even when their names are known to the public SDK.
func publicSystemSubtype(subtype string) string {
	switch subtype {
	case "init", "status", "api_retry", "thinking_tokens", "informational", "notification", "session_state_changed",
		"compact_boundary", "background_tasks_changed", "commands_changed", "control_request_progress",
		"elicitation_complete", "files_persisted", "hook_started", "hook_progress", "hook_response", "local_command_output",
		"memory_recall", "mirror_error", "model_refusal_fallback", "model_refusal_no_fallback", "permission_denied", "plugin_install",
		"task_notification", "task_progress", "task_started", "task_updated", "worker_shutting_down":
		return subtype
	default:
		return "unknown"
	}
}

func publicSystemField(name string) bool {
	switch name {
	case "type", "subtype", "uuid", "session_id", "model", "permissionMode", "status", "tools", "mcp_servers", "agents", "skills", "plugins",
		"attempt", "max_retries", "retry_delay_ms", "error_status", "error", "no_response", "estimated_tokens", "estimated_tokens_delta", "user_message_uuid",
		"content", "level", "tool_use_id", "prevent_continuation", "key", "text", "priority", "color", "timeout_ms", "state",
		"compact_metadata", "commands", "request_id", "reason", "task_id", "task_type", "description", "tool_name", "agent_id",
		"parent_tool_use_id", "subagent_type", "tool_use_result", "message", "event", "result", "usage", "errors", "is_error",
		"hook_id", "hook_name", "hook_event", "stdout", "stderr", "output", "exit_code", "outcome", "summary", "output_file",
		"mode", "memories", "files", "trigger", "direction", "scope", "original_model", "fallback_model", "patch", "tasks",
		"api_refusal_category", "api_refusal_explanation", "retracted_message_uuids", "refused_user_message_uuid", "elicitation_id",
		"mcp_server_name", "decision_reason", "decision_reason_type", "plugin_name", "rate_limit_info":
		return true
	default:
		return false
	}
}

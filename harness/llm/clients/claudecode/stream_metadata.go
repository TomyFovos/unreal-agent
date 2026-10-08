package claudecode

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"math"
)

// Only the individually checked, non-executing SDK 0.3.285 system shapes below
// are accepted. Even those cannot carry tool/subagent markers or unknown fields.
// Identity and text fields are checked for JSON type then discarded unchanged.
func validateSystemMetadata(line []byte, record streamRecord, initialized bool) error {
	bad := func() error { return unknownSystemShape(line) }
	if !initialized {
		return bad()
	}
	var fields map[string]jsontext.Value
	if json.Unmarshal(line, &fields) != nil {
		return bad()
	}
	allowed := map[string]string{"type": "string", "subtype": "string", "uuid": "string", "session_id": "string"}
	required := []string{"type", "subtype", "uuid", "session_id"}
	add := func(name, typ string, mandatory bool) {
		allowed[name] = typ
		if mandatory {
			required = append(required, name)
		}
	}
	switch record.Subtype {
	case "api_retry":
		for _, name := range []string{"attempt", "max_retries", "retry_delay_ms"} {
			add(name, "number", true)
		}
		add("error_status", "number|null", true)
		add("error", "string", true)
		add("no_response", "object", false)
	case "thinking_tokens":
		add("estimated_tokens", "number", true)
		add("estimated_tokens_delta", "number", true)
		add("user_message_uuid", "string", false)
	case "informational":
		add("content", "string", true)
		add("level", "string", true)
		add("tool_use_id", "string", false)
		add("prevent_continuation", "boolean", false)
	case "notification":
		add("key", "string", true)
		add("text", "string", true)
		add("priority", "string", true)
		add("color", "string", false)
		add("timeout_ms", "number", false)
	case "session_state_changed":
		add("state", "string", true)
	default:
		return bad()
	}
	for name, raw := range fields {
		switch name {
		case "tool_use_id", "parent_tool_use_id", "subagent_type", "tool_use_result", "tool_name", "task_id", "task_type", "agent_id", "mcp_server", "mcp_server_name":
			// No tool-linked informational message is part of text-only inference.
			if raw.Kind() != 'n' && string(raw) != `""` {
				return toolStreamFailure()
			}
		}
		typ, ok := allowed[name]
		kind := fieldJSONType(raw)
		if !ok || kind != typ && !(typ == "number|null" && (kind == "number" || kind == "null")) {
			return bad()
		}
		if kind == "number" {
			var n float64
			if json.Unmarshal(raw, &n) != nil || math.IsInf(n, 0) || math.IsNaN(n) {
				return bad()
			}
		}
	}
	for _, name := range required {
		if _, ok := fields[name]; !ok {
			return bad()
		}
	}
	match := func(name string, values ...string) bool {
		var value string
		if json.Unmarshal(fields[name], &value) != nil {
			return false
		}
		for _, known := range values {
			if value == known {
				return true
			}
		}
		return false
	}
	switch record.Subtype {
	case "api_retry":
		if !match("error", "authentication_failed", "oauth_org_not_allowed", "account_on_hold", "verification_required", "billing_error", "rate_limit", "overloaded", "invalid_request", "model_not_found", "server_error", "unknown", "max_output_tokens", "cloud_credential_error") {
			return bad()
		}
		if raw, ok := fields["no_response"]; ok {
			var timing map[string]jsontext.Value
			if json.Unmarshal(raw, &timing) != nil || len(timing) != 2 || fieldJSONType(timing["waited_ms"]) != "number" || fieldJSONType(timing["retry_wait_ms"]) != "number" {
				return bad()
			}
		}
	case "informational":
		if !match("level", "info", "notice", "suggestion", "warning") {
			return bad()
		}
		var prevent bool
		if raw, ok := fields["prevent_continuation"]; ok {
			_ = json.Unmarshal(raw, &prevent)
		}
		if prevent {
			return &Error{Code: "subprocess_failure"}
		}
	case "notification":
		if !match("priority", "low", "medium", "high", "immediate") {
			return bad()
		}
	case "session_state_changed":
		// requires_action can involve a blocking CLI permission/dialog owner;
		// it is not part of the noninteractive text-only contract.
		if !match("state", "idle", "running") {
			return bad()
		}
	}
	return nil
}

package claudecode

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
)

// Claude Code 2.1.285's requiresStructuredOutput enforcement emits this exact
// isMeta user message when an assistant ends without calling the serializer.
// THe serializes isMeta as SDKUserMessage.isSynthetic. It is private protocol
// control text, not a user task, tool receipt, or executable proposal.
const structuredEnforcementReminder = "[structured-output-enforce] You MUST call the StructuredOutput tool to complete this request. Call this tool now."

func structuredReminder(line []byte) bool {
	var fields map[string]jsontext.Value
	if len(line) > 4096 || json.Unmarshal(line, &fields) != nil {
		return false
	}
	for key, value := range fields {
		switch key {
		case "type", "message", "isSynthetic", "parent_tool_use_id":
		case "uuid", "session_id", "timestamp":
			var metadata string
			if json.Unmarshal(value, &metadata) != nil || len(metadata) > 256 {
				return false
			}
		default:
			return false
		}
	}
	var typ string
	var synthetic bool
	if json.Unmarshal(fields["type"], &typ) != nil || typ != "user" || json.Unmarshal(fields["isSynthetic"], &synthetic) != nil || !synthetic || fields["parent_tool_use_id"].Kind() != 'n' {
		return false
	}
	var message map[string]jsontext.Value
	if json.Unmarshal(fields["message"], &message) != nil || len(message) != 2 {
		return false
	}
	var role, text string
	if json.Unmarshal(message["role"], &role) != nil || role != "user" {
		return false
	}
	content := message["content"]
	if content.Kind() == '"' {
		return json.Unmarshal(content, &text) == nil && text == structuredEnforcementReminder
	}
	var blocks []map[string]jsontext.Value
	if json.Unmarshal(content, &blocks) != nil || len(blocks) != 1 || len(blocks[0]) != 2 {
		return false
	}
	var blockType string
	return json.Unmarshal(blocks[0]["type"], &blockType) == nil && blockType == "text" && json.Unmarshal(blocks[0]["text"], &text) == nil && text == structuredEnforcementReminder
}

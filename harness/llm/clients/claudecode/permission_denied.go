package claudecode

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
)

// SDKPermissionDeniedMessage is a terminal, advisory denial of a CLI tool call.
// It is not can_use_tool, an MCP registration response, or an Unreal receipt.
// The SDK has already returned its rejection to the model. It supplies neither
// the original arguments nor an Unreal permission decision: do not fabricate an
// Operation/result, approve the call, or continue to a successful final response.
// Reference: @anthropic-ai/claude-agent-sdk@0.3.285/sdk.d.ts.
//
// Decode this discriminated shape BEFORE streamRecord: its message is a string,
// unlike the object on assistant/user events. Backend prose and identities are
// checked locally then discarded, never attached to the error or canonical data.
func permissionDeniedEvent(line []byte, bridge bool, names map[string]bridgeTool) (bool, error) {
	var head struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
	}
	if json.Unmarshal(line, &head) != nil || head.Type != "system" || head.Subtype != "permission_denied" {
		return false, nil
	}
	var fields map[string]jsontext.Value
	if json.Unmarshal(line, &fields) != nil || fieldJSONType(fields["message"]) != "string" {
		return true, malformedStream(streamPermissionDeniedInvalidShape)
	}
	// Some producers omit correlation fields. Missing fields can never grant
	// permission: the minimal observed shape still fails the request. Optional
	// unknown metadata is discarded, without retaining its names or values.
	for _, name := range []string{"tool_name", "tool_use_id", "agent_id", "decision_reason_type", "decision_reason_code", "decision_reason", "uuid", "session_id"} {
		if raw, ok := fields[name]; ok && fieldJSONType(raw) != "string" {
			return true, malformedStream(streamPermissionDeniedInvalidShape)
		}
	}
	code := "permission_denied"
	if bridge {
		code = "bridge_permission_denied"
	}
	e := &Error{Code: code}
	var reason, tool, call, agent string
	_ = json.Unmarshal(fields["decision_reason_type"], &reason)
	_ = json.Unmarshal(fields["tool_name"], &tool)
	_ = json.Unmarshal(fields["tool_use_id"], &call)
	_ = json.Unmarshal(fields["agent_id"], &agent)
	e.permissionReason = permissionReasonType(reason)
	// Only an exact registered SDK tool identifies an Unreal tool-call denial.
	// A reason of "rule" does not identify managed policy or tool exposure.
	// Missing/foreign/subagent identities have an unknown scope, never guesses.
	if _, owned := names[tool]; bridge && owned && call != "" && agent == "" {
		e.permissionScope = permissionScopeUnrealCall
	}
	return true, e
}

// Closed discriminants from SDKControlPermissionRequest. The event's reason
// field is an open string, so new/private values become unknown rather than UI.
type permissionReason uint8

const (
	permissionRule permissionReason = iota + 1
	permissionMode
	permissionSubcommands
	permissionPromptTool
	permissionHook
	permissionAsyncAgent
	permissionSandbox
	permissionWorkingDir
	permissionSafetyCheck
	permissionClassifier
	permissionOther
)

func permissionReasonType(value string) permissionReason {
	for _, reason := range []permissionReason{permissionRule, permissionMode, permissionSubcommands, permissionPromptTool, permissionHook, permissionAsyncAgent, permissionSandbox, permissionWorkingDir, permissionSafetyCheck, permissionClassifier, permissionOther} {
		if reason.String() == value {
			return reason
		}
	}
	return 0
}

func (r permissionReason) String() string {
	switch r {
	case permissionRule:
		return "rule"
	case permissionMode:
		return "mode"
	case permissionSubcommands:
		return "subcommandResults"
	case permissionPromptTool:
		return "permissionPromptTool"
	case permissionHook:
		return "hook"
	case permissionAsyncAgent:
		return "asyncAgent"
	case permissionSandbox:
		return "sandboxOverride"
	case permissionWorkingDir:
		return "workingDir"
	case permissionSafetyCheck:
		return "safetyCheck"
	case permissionClassifier:
		return "classifier"
	case permissionOther:
		return "other"
	default:
		return "unknown"
	}
}

type permissionScope uint8

const permissionScopeUnrealCall permissionScope = 1

// PermissionReasonType and PermissionScope expose only closed, nonsecret
// metadata. Human-readable messages use a fixed safe summary: even sanitized
// backend prose can contain accounts, paths, credentials or policy contents.
func (e *Error) PermissionReasonType() string { return e.permissionReason.String() }
func (e *Error) PermissionScope() string {
	if e.permissionScope == permissionScopeUnrealCall {
		return "unreal_tool_call"
	}
	return "unknown"
}

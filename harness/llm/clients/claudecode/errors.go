// Package claudecode runs an installed Claude Code CLI as an external
// subscription provider. Tools are text-only by default; the opt-in SDK bridge
// routes execution to Unreal. It never opens Claude credential files.
package claudecode

import (
	"strconv"

	"github.com/unreallabsai/unreal-agent/harness/credential"
)

// Error deliberately contains no subprocess output, prompt, or account data.
type Error struct {
	Code             string
	policyReason     policyReason
	toolReason       toolFailureReason
	streamReason     streamFailureReason
	streamCount      int
	systemShape      *StreamShape
	permissionReason permissionReason
	permissionScope  permissionScope
	managedOnly      bool
	allowedTools     int
	expectedTools    int
	structuredStage  structuredStage
	structuredDetail *StructuredDiagnostics
	assistantError   assistantErrorKind
}

// Closed, nonsecret diagnostics distinguish request construction from CLI
// stream isolation failures without exposing tool names, payloads or output.
type toolFailureReason uint8

const (
	toolRequestSchemas toolFailureReason = iota + 1
	toolStreamInitialization
	toolStreamOutput
)

// A numeric closed enum prevents backend strings, JSON field values or parser
// errors from becoming diagnostics. Only the fixed codes below can be exposed.
type streamFailureReason uint8

const (
	streamInitDuplicate streamFailureReason = iota + 1
	streamInitToolsNonempty
	streamInitMCPNonempty
	streamInitPluginsLoaded // Reserved; catalog presence is not execution evidence.
	streamInitPermissionMode
	streamInitMissingRequired
	streamInitNullRequired
	streamInitWrongFieldType
	streamInitAgentsInvalid
	streamInitSkillsInvalid
	streamInitModelInvalid
	streamInitUnknownShape
	streamStatusBeforeInit
	streamStatusMissingRequired
	streamStatusWrongFieldType
	streamStatusUnknownValue
	streamStatusPermissionMode
	streamStatusToolsNonempty
	streamStatusMCPNonempty
	streamStatusPluginsLoaded // Reserved; catalog presence is not execution evidence.
	streamSystemUnknownShape
	streamPermissionDeniedInvalidShape
	streamToolExecution
)

// StreamReason returns a nonsecret, closed diagnostic code, or an empty string
// for errors outside stream validation. It never returns subprocess data.
func (e *Error) StreamReason() string {
	switch e.streamReason {
	case streamInitDuplicate:
		return "init_duplicate"
	case streamInitToolsNonempty:
		return "init_tools_nonempty"
	case streamInitMCPNonempty:
		return "init_mcp_nonempty"
	case streamInitPluginsLoaded:
		return "init_plugins_loaded"
	case streamInitPermissionMode:
		return "init_permission_mode"
	case streamInitMissingRequired:
		return "init_missing_required_field"
	case streamInitNullRequired:
		return "init_null_required_field"
	case streamInitWrongFieldType:
		return "init_wrong_field_type"
	case streamInitAgentsInvalid:
		return "init_agents_invalid"
	case streamInitSkillsInvalid:
		return "init_skills_invalid"
	case streamInitModelInvalid:
		return "init_model_invalid"
	case streamInitUnknownShape:
		return "init_unknown_shape"
	case streamStatusBeforeInit:
		return "status_before_init"
	case streamStatusMissingRequired:
		return "status_missing_required_field"
	case streamStatusWrongFieldType:
		return "status_wrong_field_type"
	case streamStatusUnknownValue:
		return "status_unknown_value"
	case streamStatusPermissionMode:
		return "status_permission_mode"
	case streamStatusToolsNonempty:
		return "status_tools_nonempty"
	case streamStatusMCPNonempty:
		return "status_mcp_nonempty"
	case streamStatusPluginsLoaded:
		return "status_plugins_loaded"
	case streamSystemUnknownShape:
		return "system_unknown_shape"
	case streamToolExecution:
		return "stream_tool_execution"
	case streamPermissionDeniedInvalidShape:
		return "permission_denied_invalid_shape"
	default:
		return ""
	}
}

func (e *Error) withStreamReason(message string) string {
	reason := e.StreamReason()
	if reason == "" {
		return message
	}
	// Put the short reason first so a bounded/narrow failure display does not
	// hide it behind the existing descriptive isolation diagnostic.
	if e.streamCount > 0 {
		reason += "(count=" + strconv.Itoa(e.streamCount) + ")"
	}
	if e.systemShape != nil {
		return reason + ": " + e.systemShape.summary() + ": " + message
	}
	return reason + ": " + message
}

func (e *Error) Error() string {
	message := e.message()
	if stage := e.StructuredStage(); stage != "" {
		// Keep the stage first: even a narrow/clipped failure view can identify
		// the branch. Everything in this suffix is closed schema metadata.
		message = "stage=" + stage + ": " + message
		if e.structuredDetail != nil {
			message += " (" + e.structuredDetail.summary() + ")"
		} else if reason := e.AssistantReason(); reason != "" {
			message += " (reason=" + reason + ")"
		}
	} else if reason := e.AssistantReason(); reason != "" {
		message += " (reason=" + reason + ")"
	}
	return message
}

func (e *Error) message() string {
	switch e.Code {
	case "provider_request_rejected":
		return "Claude Code rejected the provider request"
	case "generation_output_limit":
		return "Claude Code generation reached its output token limit; no validated completion was accepted"
	case "subprocess_failure":
		switch e.assistantError {
		case assistantOverloaded:
			return "Claude Code provider is temporarily overloaded; retry requires an explicit user action"
		case assistantServerError:
			return "Claude Code provider reported a server error; retry requires an explicit user action"
		case assistantUnknown:
			return "Claude Code provider reported an unspecified assistant error"
		case assistantErrorUnrecognized:
			return "Claude Code provider returned an unrecognized assistant error"
		default:
			return "Claude Code subprocess failed"
		}
	case "structured_unavailable":
		return "Claude StructuredOutput is unavailable or denied by provider policy; no fallback (decision_reason_type=" + e.PermissionReasonType() + ")"
	case "structured_protocol_invalid":
		return "Claude structured action protocol validation failed"
	case "structured_response_too_large":
		return "Claude structured response exceeded the size limit"
	case "structured_generation_timeout":
		return "Claude structured generation timed out"
	case "structured_serializer_round_limit":
		return "Claude StructuredOutput reached its bounded serializer turn limit; no Action or Final was accepted"
	case "structured_runtime_changed":
		return "Claude structured action runtime changed during a pinned turn"
	case "action_round_limit":
		return "Claude structured action loop reached MaxActionRounds; canonical receipts are retained"
	case "bridge_allowlist_inactive":
		if e.managedOnly {
			return "Claude tool bridge allowlist is inactive: organization requires managed permission rules (allowed=" + strconv.Itoa(e.allowedTools) + "/" + strconv.Itoa(e.expectedTools) + "); ask your administrator to allow the exact Unreal SDK MCP tools"
		}
		return "Claude tool bridge allowlist is inactive (allowed=" + strconv.Itoa(e.allowedTools) + "/" + strconv.Itoa(e.expectedTools) + "); no user input was sent"
	case "permission_denied", "bridge_permission_denied":
		prefix := "Claude Code"
		if e.Code == "bridge_permission_denied" {
			prefix = "Claude tool bridge"
		}
		return prefix + ": provider permission denied (decision_reason_type=" + e.PermissionReasonType() + "; scope=" + e.PermissionScope() + ")"
	case "bridge_configuration_invalid":
		return "Claude tool bridge configuration is invalid"
	case "bridge_context_budget":
		return "Claude tool bridge reached the bounded continuation context budget; canonical receipts are retained"
	case "bridge_host_required":
		return "Claude tool bridge requires an Unreal-owned tool handler"
	case "bridge_schema_invalid":
		return "Claude tool bridge schema is invalid"
	case "bridge_unknown_tool":
		return "Claude tool bridge rejected an unknown tool"
	case "bridge_arguments_invalid":
		return "Claude tool bridge rejected malformed tool arguments"
	case "bridge_duplicate_conflict":
		return "Claude tool bridge rejected a conflicting duplicate request"
	case "bridge_recovery_required":
		return "Claude action requires canonical Operation recovery; it was not executed again"
	case "bridge_ambiguous_call":
		return "Claude tool bridge cannot safely correlate tool request identity"
	case "bridge_round_limit":
		return "Claude tool bridge reached the configured tool round limit"
	case "bridge_tool_timeout":
		return "Claude tool bridge timed out waiting for an Unreal tool receipt"
	case "bridge_tool_canceled":
		return "Claude tool bridge tool request was canceled"
	case "bridge_receipt_invalid":
		return "Claude tool bridge received an invalid Unreal tool receipt"
	case "bridge_unavailable":
		return "Claude tool bridge SDK transport is unavailable"
	case "bridge_host_failure":
		return "Claude tool bridge host request failed"
	case "bridge_protocol_failure":
		return "Claude tool bridge protocol failed"
	case "external_reauth_required":
		return "Claude Code is not signed in. Run: claude auth login"
	case "binary_not_found":
		return "Claude Code executable was not found; configure the installed binary path"
	case "unsupported_version":
		return "Claude Code CLI does not support the required isolation/streaming contract"
	case "isolation_contract_invalid":
		return "Claude Code launch isolation contract is invalid; subprocess was not started"
	case "subscription_unavailable":
		return "Claude Code subscription authentication is unavailable; API authentication is not supported by claude-code"
	case "subscription_mode_conflict":
		return "Claude Code subscription mode conflicts with alternative provider/billing environment settings"
	case "invalid_managed_policy_mode":
		return "Claude Code managedPolicyMode must be reject or trust"
	case "policy_isolation_unavailable":
		switch e.policyReason {
		case policySourcePresent:
			return "Claude Code managed policy source is present; executable policy cannot be ruled out"
		case policyRemoteMutable:
			return "Claude Code can fetch and apply managed policy during this request; a prior diagnostic cannot guarantee policy isolation"
		case policyStateUnknown:
			return "Claude Code managed policy absence cannot be verified; policy isolation is unavailable"
		default:
			return "Claude Code managed policy isolation is unavailable; policy hooks cannot be disabled"
		}
	case "tools_unsupported":
		switch e.toolReason {
		case toolRequestSchemas:
			return "claude-code is text-only: tools unsupported (request included tool schemas)"
		case toolStreamInitialization:
			return e.withStreamReason("claude-code is text-only: tools unsupported (Claude stream initialization violated isolation)")
		case toolStreamOutput:
			return e.withStreamReason("claude-code is text-only: tools unsupported (Claude stream contained non-text tool output)")
		}
		return "claude-code is text-only: tools unsupported"
	case "invalid_model":
		return "Claude Code model is unavailable or is absent from the selectable catalog"
	case "catalog_unavailable":
		return "Claude Code model catalog discovery is unavailable"
	case "invalid_effort":
		return "Claude Code reasoning effort is unavailable for the selected model"
	case "rate_limited":
		return "Claude Code subscription rate or usage limit reached"
	case "malformed_stream":
		return e.withStreamReason("Claude Code returned an invalid or incomplete stream")
	case "unsupported_input":
		return "Claude Code text-only provider cannot represent this input"
	default:
		return "Claude Code subprocess failed"
	}
}

func (e *Error) Unwrap() error {
	if e.Code == "external_reauth_required" {
		return &credential.Error{Code: "external_reauth_required"}
	}
	return nil
}

package operation

import "github.com/unreallabsai/unreal-agent/harness/permission"

// Authorize checks current policy before starting or resuming an operation.
// Custom handlers must additionally enforce their concrete file/process/HTTP
// effects at the execution boundary using the same policy.
func Authorize(policy *permission.Policy, current Operation) error {
	if current.ToolName != "" {
		if err := policy.CheckTool(current.ToolName); err != nil {
			return err
		}
	}
	switch current.Type {
	case TypeValue:
		return nil
	case TypeShell:
		if err := policy.CheckTool("Bash"); err != nil {
			return err
		}
		return policy.CheckProcess()
	case TypeViewImage:
		if err := policy.CheckTool("ViewImage"); err != nil {
			return err
		}
		state, err := DecodeViewImageState(current)
		if err != nil {
			return err
		}
		return policy.CheckPath(state.Path, false)
	case TypeSkillUse:
		if err := policy.CheckTool("SkillUse"); err != nil {
			return err
		}
		state, err := DecodeSkillUse(current)
		if err != nil {
			return err
		}
		return policy.CheckPath(state.Path, false)
	case TypeRemoteJob:
		state, err := DecodeRemoteJobState(current)
		if err != nil {
			return err
		}
		if current.ToolName == "" {
			return policy.CheckTool(string(state.Plan.Type))
		}
		return nil
	default:
		if current.ToolName == "" {
			return policy.CheckTool(string(current.Type))
		}
		return nil
	}
}

// PermissionFailure converts an expected denial into a durable terminal value.
// Callers must not route permission denials through Manager.Add's fatal error.
func PermissionFailure(current Operation, denial *permission.Error) Operation {
	copy := *denial
	current.Denial = &copy
	// Preserve existing result formats for the built-in translators, but do not
	// panic on a malformed historical payload when denying it before execution.
	switch current.Type {
	case TypeShell:
		if _, err := NewShell(current); err == nil {
			return failLocalOperation(current, denial)
		}
	case TypeViewImage:
		if _, err := DecodeViewImageState(current); err == nil {
			return failLocalOperation(current, denial)
		}
	case TypeSkillUse:
		if _, err := DecodeSkillUse(current); err == nil {
			return failLocalOperation(current, denial)
		}
	case TypeRemoteJob:
		if _, err := DecodeRemoteJobState(current); err == nil {
			return failLocalOperation(current, denial)
		}
	}
	current.Status = StatusFailed
	return current
}

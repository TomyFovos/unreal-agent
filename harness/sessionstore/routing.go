package sessionstore

import (
	"github.com/unreallabsai/unreal-agent/harness/contextstate"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
)

// AutomaticPermissionRule classifies the A9 routing outcome from the existing
// Permission receipt. A parent denial carries the approval capability.
func AutomaticPermissionRule(op operation.Operation) string {
	if op.Permission == nil || op.Type == operation.TypeValue || op.Status == operation.StatusCanceled {
		return ""
	}
	if op.Type == operation.TypeRemoteJob {
		if state, err := operation.DecodeRemoteJobState(op); err != nil || state.Plan.Type == "subagent" || state.Plan.Type == contextstate.OperationType {
			return ""
		}
	}
	state := op.Permission.State
	automatic := state == "read_only" || state == "full_access"
	if op.Denial != nil && op.Denial.Code == permission.Denied && (automatic || state == "denied" && op.Denial.Capability != "approval") {
		return "A2"
	}
	if op.Denial == nil && automatic {
		return "A1"
	}
	return ""
}

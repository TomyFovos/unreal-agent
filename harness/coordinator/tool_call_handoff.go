package coordinator

import (
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

// scheduledToolCall carries only the owner-local dispatch and grace decisions.
// The full ToolCallStatus remains a value at the canonical Store and observer
// boundaries. In particular, inherited inspection facts and the nested status
// are not copied into another returned slice for every ordinary tool call.
//
// Operations retains the existing immutable slice handoff: construction and
// persistence complete before processModelResponse dispatches any Operation.
// Workers still receive the Manager's detached execution-metadata snapshots.
type scheduledToolCall struct {
	TurnID             session.TurnID
	CallID             string
	Operations         []operation.Operation
	needsModelResponse bool
}

func toolCallHandoff(status sessionstore.ToolCallStatus) scheduledToolCall {
	return scheduledToolCall{
		TurnID:             status.TurnID,
		CallID:             status.CallID,
		Operations:         status.Operations,
		needsModelResponse: status.Status.Error != "" || len(status.Status.WaitingFor) == 0,
	}
}

func toolCallStatusesRequireModelResponse(statuses []scheduledToolCall) bool {
	for _, status := range statuses {
		if status.needsModelResponse {
			return true
		}
	}
	return false
}

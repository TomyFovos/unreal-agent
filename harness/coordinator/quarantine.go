package coordinator

import (
	"encoding/json/jsontext"
	"encoding/json/v2"

	"github.com/unreallabsai/unreal-agent/harness/mutation"
	files "github.com/unreallabsai/unreal-agent/harness/native"
	"github.com/unreallabsai/unreal-agent/harness/operation"
)

const quarantineMaxStateBytes = 32 << 20

// operationObservational proves an execution kind, never a tool's claimed name
// or a permission grant. Permission must still authorize every actual access.
func operationObservational(op operation.Operation) bool {
	if op.Type == operation.TypeValue && op.Version == operation.VersionValue {
		var value operation.ValueState
		return quarantineDecode(op.State, &value) && len(value.Value) > 0 && value.Value.IsValid()
	}
	_, request, ok := quarantineNativePlan(op)
	return ok && (request.Action == "read" || request.Action == "grep" || request.Action == "glob")
}

// operationUncertain examines canonical typed evidence only. Its nonterminal
// case is for recovered work whose former executor lifetime has ended; a live
// pending/Ask operation must not be relabeled uncertain merely by calling this.
// Unknown plans and missing or malformed outcome evidence fail closed.
func operationUncertain(op operation.Operation) bool {
	if operationObservational(op) {
		return false
	}
	if !quarantineJSON(op.State) {
		return true
	}
	if !operationIsTerminal(op.Status) {
		return true
	}
	if op.Type == operation.TypeShell {
		var state operation.ShellState
		if op.Version != operation.VersionShell || !quarantineDecode(op.State, &state) {
			return true
		}
		if _, err := operation.NewShell(op); err != nil {
			return true
		}
		if state.Result != nil {
			// A typed terminal process result is evidence of completion, even
			// when the command exited unsuccessfully. It grants no retry.
			return false
		}
		return !quarantineAdmissionDenied(op) || state.Phase != "" || state.ProcessGroupID != 0 || state.PendingExitCode != nil
	}
	if state, request, ok := quarantineNativePlan(op); ok {
		if quarantineUnstartedRemote(state) && quarantineAdmissionDenied(op) {
			return false
		}
		var result files.Result
		if !quarantineDecode(state.Handle, &result) || result.Version != files.Version || result.Path != request.Path || result.Mutation == nil || result.Code != string(result.Mutation.Code) {
			return true
		}
		return quarantineMutationOutcome(*result.Mutation, []string{request.Path}, request.Action == "edit")
	}
	if op.Type == operation.TypeRemoteJob {
		state, err := operation.DecodeRemoteJobState(op)
		if err != nil || state.Plan.Type != mutation.PlanType || state.Plan.Version != mutation.Version {
			return true
		}
		var request mutation.Request
		if !quarantineDecode(state.Plan.Data, &request) || request.Validate() != nil {
			return true
		}
		if quarantineUnstartedRemote(state) && quarantineAdmissionDenied(op) {
			return false
		}
		var result mutation.Result
		if !quarantineDecode(state.Handle, &result) {
			return true
		}
		paths := make([]string, len(request.Changes))
		for index, change := range request.Changes {
			paths[index] = change.Path
		}
		return quarantineMutationOutcome(result, paths, false)
	}
	return true
}

func quarantineUnstartedRemote(state operation.RemoteJobState) bool {
	return len(state.Handle) == 0 && len(state.Subscription) == 0 && len(state.OutstandingInput) == 0 && state.NextInspectionAt.IsZero() && state.TerminalResult == ""
}

// operationsIndependent is the conservative structural check for callers
// without concrete executor evidence. It does no filesystem I/O or execution.
// Effect admission uses prepareRecoveryOperation plus the actual handler's
// canonical footprint, prerequisite and Mutation-boundary proofs; this helper
// never promotes pathname inequality into such a proof.
func operationsIndependent(candidate operation.Operation, unresolved []operation.Operation) bool {
	if len(unresolved) == 0 {
		return true
	}
	for _, prior := range unresolved {
		if candidate.ID != "" && candidate.ID == prior.ID {
			return false
		}
	}
	// The owner supplies its incrementally derived quarantine, not arbitrary
	// history. Do not repeatedly decode old, potentially large mutation plans
	// for each harmless observation. An unexpected extra entry stays closed.
	return operationObservational(candidate)
}

func quarantineNativePlan(op operation.Operation) (operation.RemoteJobState, files.Request, bool) {
	if !quarantineJSON(op.State) {
		return operation.RemoteJobState{}, files.Request{}, false
	}
	state, err := operation.DecodeRemoteJobState(op)
	if err != nil || state.Plan.Type != files.PlanType || state.Plan.Version != files.Version {
		return operation.RemoteJobState{}, files.Request{}, false
	}
	var request files.Request
	if !quarantineDecode(state.Plan.Data, &request) || request.Validate() != nil || op.ToolName != "" && op.ToolName != request.Action {
		return operation.RemoteJobState{}, files.Request{}, false
	}
	return state, request, true
}

func quarantineAdmissionDenied(op operation.Operation) bool {
	if op.Denial == nil || op.Permission == nil || !operationIsTerminal(op.Status) {
		return false
	}
	// These decisions are produced before LocalOperationManager.startOperation.
	// "expired" is deliberately excluded: a prior Host may have executed work
	// before its first durable progress checkpoint.
	switch op.Permission.State {
	case "denied", "timeout", "canceled", "revoked":
		return true
	default:
		return false
	}
}

func quarantineMutationOutcome(result mutation.Result, paths []string, prepared bool) bool {
	if result.Version != mutation.Version || result.Code == mutation.Indeterminate {
		return true
	}
	switch result.Code {
	case mutation.Applied, mutation.Stale, mutation.Invalid, mutation.Denied, mutation.Failed, mutation.Canceled,
		"binary", "no_match", "not_found", "ambiguous", "read_failed":
	default:
		return true
	}
	if len(result.Targets) != len(paths) {
		// Edit preparation can refuse before it has built a mutation request.
		return !(prepared && result.Code != mutation.Applied && len(result.Targets) == 0)
	}
	for index, target := range result.Targets {
		if target.Path != paths[index] || target.Revision.Validate() != nil {
			return true
		}
		switch target.Code {
		case mutation.Applied:
			if !target.Revision.Exists || result.Code != mutation.Applied && result.Code != mutation.Failed && result.Code != mutation.Canceled {
				return true
			}
		case mutation.NotApplied, mutation.Stale, mutation.Invalid, mutation.Denied, mutation.Failed, mutation.Canceled:
			if result.Code == mutation.Applied {
				return true
			}
		default:
			return true
		}
	}
	return false
}

func quarantineDecode(raw []byte, destination any) bool {
	return quarantineJSON(raw) && json.Unmarshal(raw, destination, json.RejectUnknownMembers(true)) == nil
}

func quarantineJSON(raw []byte) bool {
	if len(raw) == 0 || len(raw) > quarantineMaxStateBytes {
		return false
	}
	depth, quoted, escaped := 0, false, false
	for _, ch := range raw {
		if quoted {
			if escaped {
				escaped = false
			} else if ch == '\\' {
				escaped = true
			} else if ch == '"' {
				quoted = false
			}
			continue
		}
		switch ch {
		case '"':
			quoted = true
		case '{', '[':
			depth++
			if depth > 64 {
				return false
			}
		case '}', ']':
			depth--
		}
	}
	return jsontext.Value(raw).IsValid()
}

package coordinator

import (
	"context"
	"slices"

	files "github.com/unreallabsai/unreal-agent/harness/native"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
)

func recoveryDenial(reason string) *permission.Error {
	return &permission.Error{Code: permission.Denied, Capability: "recovery_dependency", Reason: reason}
}

func (current *coordinator) prepareRecoveryOperation(ctx context.Context, candidate operation.Operation) (operation.Operation, *permission.Error) {
	// Admission inspects inert plans only. The executor obtains a fresh lease,
	// captures its footprint asynchronously and waits for the owner's durable ACK.
	if backend, ok := current.dependencies.Operations.(operation.ExecutionPreparationRequirement); ok && backend.RequiresExecutionPreparation(candidate) {
		candidate = operation.WithPreparationRequired(candidate, true)
	}
	if current.nativeConversation() && (candidate.ToolName == "write" || candidate.ToolName == "edit" || candidate.ToolName == "") {
		if candidate.RequiresPreparation() {
			// Keep detached execution metadata without decoding the same inert
			// header again. The worker still strictly validates the full plan.
			candidate = operation.WithPreparationRequired(candidate, true)
		} else if state, err := operation.DecodeRemoteJobState(candidate); err == nil && state.Plan.Type == files.PlanType && state.Plan.Version == files.Version {
			candidate = operation.WithPreparationRequired(candidate, true)
		}
	}
	state := current.conversation
	if state == nil {
		return candidate, nil
	}
	if state.overflow {
		return candidate, recoveryDenial("bounded recovery evidence requires reconciliation")
	}
	if len(state.quarantined) == 0 && len(state.pending) == 0 {
		return candidate, nil
	}
	if state.quarantined[candidate.ID] {
		return candidate, recoveryDenial("uncertain canonical operation cannot be retried")
	}
	observational := operationObservational(candidate)
	if observational && len(candidate.DependencyIDs()) == 0 {
		return candidate, nil
	}
	current.drainUncertainty()
	if len(state.quarantined) == 0 {
		return candidate, nil
	}
	if !current.dependenciesCertain(candidate) {
		return candidate, recoveryDenial("execution prerequisite is uncertain or unavailable")
	}
	if observational {
		return candidate, nil
	}
	p, ok := current.dependencies.Operations.(operation.RecoveryPlanner)
	if !ok {
		return candidate, recoveryDenial("independent executor evidence unavailable")
	}
	ids := make([]operation.ID, 0, len(state.quarantined))
	for id := range state.quarantined {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	effects := make([]operation.UncertainEffect, 0, len(ids))
	for _, id := range ids {
		basis := state.effects[id]
		if len(basis) == 0 {
			return candidate, recoveryDenial("historical effect footprint unavailable")
		}
		effects = append(effects, operation.UncertainEffect{ID: id, Basis: basis})
	}
	prepared, err := p.PlanRecovery(ctx, candidate, effects)
	if err != nil {
		return candidate, recoveryDenial("effect footprint or prerequisite independence unproven")
	}
	return prepared, nil
}

// Dependencies are Host-side execution facts, not a tool's self-asserted
// independence flag. A native literal write has a finite executor read set
// (the target Expected revision), which the executor separately verifies.
// This proves execution-effect separation, not semantic code independence.
func (current *coordinator) dependenciesCertain(candidate operation.Operation) bool {
	if len(candidate.DependencyIDs()) > 128 {
		return false
	}
	visited, visiting := map[operation.ID]bool{}, map[operation.ID]bool{}
	count := 0
	var check func(operation.ID) bool
	check = func(id operation.ID) bool {
		if id == "" || id == candidate.ID || current.quarantined(id) || visiting[id] {
			return false
		}
		if visited[id] {
			return true
		}
		count++
		if count > 128 {
			return false
		}
		op, exists := current.state.operations[id]
		if !exists || !operationIsTerminal(op.Status) || operationUncertain(op) || len(op.DependencyIDs()) > 128 {
			return false
		}
		visiting[id] = true
		for _, prior := range op.DependencyIDs() {
			if !check(prior) {
				return false
			}
		}
		delete(visiting, id)
		visited[id] = true
		return true
	}
	for _, id := range candidate.DependencyIDs() {
		if !check(id) {
			return false
		}
	}
	return true
}

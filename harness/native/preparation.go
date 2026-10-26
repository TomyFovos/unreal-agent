package native

import (
	"context"
	"errors"

	"github.com/unreallabsai/unreal-agent/harness/mutation"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
)

// preparationFailure is used only before Executor.Execute has been called.
// Its typed NotApplied targets distinguish a refused new proposal from an
// unknown outcome after execution. It must never wrap an executor failure.
func preparationFailure(ctx context.Context, request Request, err error) Result {
	code := mutation.Denied
	denial := permission.Failure(err)
	reason := mutation.ReasonPermissionDenied
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		code, reason = mutation.Canceled, ""
	} else if denial == nil {
		denial = &permission.Error{Code: permission.Denied, Capability: "recovery_dependency", Reason: "authorized execution preparation unavailable"}
	}
	result := Result{Version: Version, Code: string(code), Path: request.Path, Denial: denial, Reason: reason}
	if request.Action == "write" || request.Action == "edit" {
		result.Mutation = &mutation.Result{Version: mutation.Version, Code: code, Denial: denial, Reason: reason, Targets: []mutation.TargetResult{{Path: request.Path, Code: mutation.NotApplied}}}
	}
	return result
}

// PlanRecovery validates inert typed plans and canonical constraints only.
// It is not an independence proof or permission to touch the filesystem.
func (h *Handler) PlanRecovery(ctx context.Context, current operation.Operation, blocked []operation.UncertainEffect) (operation.Operation, error) {
	if ctx.Err() != nil || len(blocked) == 0 || len(blocked) > 128 {
		return current, operation.ErrIndependenceUnproven
	}
	r, _, err := recoveryRequest(current)
	if err != nil || r.Action != "write" && r.Action != "edit" {
		return current, operation.ErrIndependenceUnproven
	}
	path, err := h.executor.Files.Path(r.Path)
	if err != nil {
		return current, operation.ErrIndependenceUnproven
	}
	seen := map[operation.ID]bool{}
	for _, effect := range blocked {
		if effect.ID == "" || effect.ID == current.ID || seen[effect.ID] {
			return current, operation.ErrIndependenceUnproven
		}
		seen[effect.ID] = true
		basis, err := h.decodeBasis(effect.Basis)
		if err != nil {
			return current, operation.ErrIndependenceUnproven
		}
		for _, target := range basis.Footprint.Targets {
			if target.Path == path {
				return current, operation.ErrIndependenceUnproven
			}
		}
	}
	return operation.WithRecoveryBasis(operation.WithPreparationRequired(current, true), blocked), nil
}

func (h *Handler) RemoteJobPreparations() <-chan operation.PreparationRequest { return h.preparations }

func (h *Handler) prepareWorker(ctx context.Context, current operation.Operation) (operation.Operation, context.Context, error) {
	return h.prepareWorkerWithRegisteredPlan(ctx, current, nil)
}

func (h *Handler) prepareWorkerWithRegisteredPlan(ctx context.Context, current operation.Operation, registered *registeredPreparation) (operation.Operation, context.Context, error) {
	prepared := current
	var decoded recoveryPlan
	var plan *recoveryPlan
	var err error
	if current.RequiresPreparation() {
		// Decode strictly once after authorization. Both preparation helpers use
		// this worker-owned plan before the durable owner acknowledgement.
		if registered != nil {
			if registered.err != nil {
				return current, ctx, registered.err
			}
			plan = &registered.plan
		} else {
			decoded, err = decodeRecoveryPlan(current)
			if err != nil {
				return current, ctx, err
			}
			plan = &decoded
		}
		prepared, err = h.prepareExecutionWithPlan(ctx, current, plan)
		if err != nil {
			return current, ctx, err
		}
	}
	executionContext, err := h.recoveryExecutionContextWithPlan(ctx, prepared, plan)
	if err != nil {
		return current, ctx, err
	}
	if !current.RequiresPreparation() {
		return prepared, executionContext, nil
	}
	result := make(chan operation.PreparationResult, 1)
	request := operation.PreparationRequest{ID: current.ID, Basis: prepared.ExecutionBasis().Clone(), Result: result}
	select {
	case h.preparations <- request:
	case <-ctx.Done():
		return current, ctx, ctx.Err()
	}
	// Once accepted, cancellation is answered by the manager with its exact
	// metadata checkpoint. No context branch may race that checkpoint locally.
	select {
	case reply := <-result:
		if reply.Operation.ID != "" {
			current = reply.Operation
		}
		if reply.Err != nil {
			return current, ctx, reply.Err
		}
		if ctx.Err() != nil {
			return current, ctx, ctx.Err()
		}
		return current, executionContext, nil
	case <-h.ctx.Done():
		return current, ctx, h.ctx.Err()
	}
}

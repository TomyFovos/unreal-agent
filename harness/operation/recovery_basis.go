package operation

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"errors"
	"slices"
)

// UncertainEffect is a bounded, non-executable derivative of canonical evidence.
// It carries no parent Operation authority. A missing/invalid Basis remains
// unknown and cannot grant independent effects.
type UncertainEffect struct {
	ID    ID
	Basis jsontext.Value `json:",omitzero"`
}

// ExecutionPreparer binds an executor's actual finite footprint before the
// existing canonical Operation publication. It grants no Permission.
type ExecutionPreparer interface {
	PrepareExecution(context.Context, Operation) (Operation, error)
}

// RecoveryPreparer must prove concrete execution preconditions/effects, not
// pathname inequality or a model's declaration of independence.
type RecoveryPreparer interface {
	PrepareRecovery(context.Context, Operation, []UncertainEffect) (Operation, error)
}

var ErrIndependenceUnproven = errors.New("operation independence unproven")

func (manager *LocalOperationManager) recoveryHandler(current Operation) RemoteJobHandler {
	state, err := DecodeRemoteJobState(current)
	if err != nil {
		return nil
	}
	var match RemoteJobHandler
	for _, h := range manager.remoteJobs.handlers {
		if h == nil || h.RemoteJobPlanType() != state.Plan.Type || h.RemoteJobPlanVersion() != state.Plan.Version {
			continue
		}
		if match != nil {
			return nil
		}
		match = h
	}
	return match
}

func (manager *LocalOperationManager) PrepareExecution(ctx context.Context, current Operation) (Operation, error) {
	if h, ok := manager.recoveryHandler(current).(ExecutionPreparer); ok {
		return h.PrepareExecution(ctx, current)
	}
	return current, nil
}
func (manager *LocalOperationManager) PrepareRecovery(ctx context.Context, current Operation, blocked []UncertainEffect) (Operation, error) {
	if len(blocked) == 0 {
		return current, nil
	}
	if h, ok := manager.recoveryHandler(current).(RecoveryPreparer); ok {
		return h.PrepareRecovery(ctx, current, blocked)
	}
	return current, ErrIndependenceUnproven
}

// ExecutionMetadata is absent on ordinary operations. Field names remain flat
// in canonical JSON via Operation.Execution's json/v2 embed tag.
type ExecutionMetadata struct {
	ExecutionBasis      jsontext.Value    `json:",omitzero"`
	RecoveryBasis       []UncertainEffect `json:",omitzero"`
	Dependencies        []ID              `json:",omitzero"`
	PreparationRequired bool              `json:",omitzero"`
}

// IdempotencyMetadata keeps the optional idempotency value out of ordinary
// Operation copies. Its trailing embed preserves the flat canonical field order.
type IdempotencyMetadata struct {
	Idempotency jsontext.Value `json:",omitzero"`
}

// Prerequisites can carry Host dependencies only. Spec cannot supply executor
// footprints or independent-effect proof as authority.
type Prerequisites struct {
	Dependencies []ID `json:",omitzero"`
}

func (current Operation) ExecutionBasis() jsontext.Value {
	if current.Execution == nil {
		return nil
	}
	return current.Execution.ExecutionBasis
}
func (current Operation) RecoveryBasis() []UncertainEffect {
	if current.Execution == nil {
		return nil
	}
	return current.Execution.RecoveryBasis
}
func (current Operation) DependencyIDs() []ID {
	if current.Execution == nil {
		return nil
	}
	return current.Execution.Dependencies
}
func (current Spec) DependencyIDs() []ID {
	if current.Prerequisites == nil {
		return nil
	}
	return current.Prerequisites.Dependencies
}

func (current Operation) Idempotency() jsontext.Value {
	if current.Idempotence == nil {
		return nil
	}
	return current.Idempotence.Idempotency
}

// RequiresPreparation is a Host execution constraint, never a permission grant.
func (current Operation) RequiresPreparation() bool {
	return current.Execution != nil && current.Execution.PreparationRequired
}

// CloneExecutionMetadata keeps the nil fast path allocation-free and gives a
// worker independent ownership of both the container and every mutable slice.
func CloneExecutionMetadata(current Operation) Operation {
	if current.Execution != nil {
		current.Execution = cloneExecutionMetadata(current.Execution)
	}
	if current.Idempotence != nil {
		current.Idempotence = &IdempotencyMetadata{Idempotency: current.Idempotency().Clone()}
	}
	return current
}

func cloneExecutionMetadata(metadata *ExecutionMetadata) *ExecutionMetadata {
	value := *metadata
	value.ExecutionBasis = value.ExecutionBasis.Clone()
	value.Dependencies = slices.Clone(value.Dependencies)
	if value.RecoveryBasis != nil {
		effects := make([]UncertainEffect, len(value.RecoveryBasis))
		for i, e := range value.RecoveryBasis {
			effects[i] = UncertainEffect{ID: e.ID, Basis: e.Basis.Clone()}
		}
		value.RecoveryBasis = effects
	}
	return &value
}

// These setters return a detached copy. Existing canonical metadata and worker
// comparison baselines must never be mutated through a shallow pointer copy.
func WithIdempotency(current Operation, value jsontext.Value) Operation {
	if current.Idempotence == nil && value == nil {
		return current
	}
	if current.Execution != nil {
		current.Execution = cloneExecutionMetadata(current.Execution)
	}
	current.Idempotence = &IdempotencyMetadata{Idempotency: value.Clone()}
	return current
}

func WithPreparationRequired(current Operation, required bool) Operation {
	if current.Execution == nil && !required {
		return current
	}
	current = CloneExecutionMetadata(current)
	if current.Execution == nil {
		current.Execution = &ExecutionMetadata{}
	}
	current.Execution.PreparationRequired = required
	return current
}

func WithExecutionBasis(current Operation, basis jsontext.Value) Operation {
	if current.Execution == nil && basis == nil {
		return current
	}
	current = CloneExecutionMetadata(current)
	if current.Execution == nil {
		current.Execution = &ExecutionMetadata{}
	}
	current.Execution.ExecutionBasis = basis.Clone()
	return current
}
func WithRecoveryBasis(current Operation, effects []UncertainEffect) Operation {
	if current.Execution == nil && effects == nil {
		return current
	}
	current = CloneExecutionMetadata(current)
	if current.Execution == nil {
		current.Execution = &ExecutionMetadata{}
	}
	if effects == nil {
		current.Execution.RecoveryBasis = nil
		return current
	}
	current.Execution.RecoveryBasis = make([]UncertainEffect, len(effects))
	for i, e := range effects {
		current.Execution.RecoveryBasis[i] = UncertainEffect{ID: e.ID, Basis: e.Basis.Clone()}
	}
	return current
}
func WithDependencies(current Operation, dependencies []ID) Operation {
	if current.Execution == nil && dependencies == nil {
		return current
	}
	return withDependencies(current, dependencies)
}

func withDependencies(current Operation, dependencies []ID) Operation {
	current = CloneExecutionMetadata(current)
	if current.Execution == nil {
		current.Execution = &ExecutionMetadata{}
	}
	current.Execution.Dependencies = slices.Clone(dependencies)
	return current
}
func WithPrerequisites(current Spec, dependencies []ID) Spec {
	if current.Prerequisites == nil && dependencies == nil {
		return current
	}
	current.Prerequisites = &Prerequisites{Dependencies: slices.Clone(dependencies)}
	return current
}

func SameExecutionMetadata(a, b Operation) bool {
	if !bytes.Equal(a.Idempotency(), b.Idempotency()) {
		return false
	}
	if a.Execution == nil && b.Execution == nil {
		return true
	}
	return sameExecutionMetadata(a.Execution, b.Execution)
}

func sameExecutionMetadata(a, b *ExecutionMetadata) bool {
	if a == nil {
		a = &ExecutionMetadata{}
	}
	if b == nil {
		b = &ExecutionMetadata{}
	}
	if a.PreparationRequired != b.PreparationRequired || !bytes.Equal(a.ExecutionBasis, b.ExecutionBasis) || !slices.Equal(a.Dependencies, b.Dependencies) || len(a.RecoveryBasis) != len(b.RecoveryBasis) {
		return false
	}
	for i, e := range a.RecoveryBasis {
		if e.ID != b.RecoveryBasis[i].ID || !bytes.Equal(e.Basis, b.RecoveryBasis[i].Basis) {
			return false
		}
	}
	return true
}

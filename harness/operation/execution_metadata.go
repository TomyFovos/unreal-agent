package operation

import (
	"bytes"
	"encoding/json/jsontext"
	"slices"
)

// ExecutionMetadata is absent on ordinary operations. Field names remain flat
// in canonical JSON via Operation.Execution's json/v2 embed tag.
type ExecutionMetadata struct {
	Dependencies []ID `json:",omitzero"`
}

// IdempotencyMetadata keeps the optional idempotency value out of ordinary
// Operation copies. Its trailing embed preserves the flat canonical field order.
type IdempotencyMetadata struct {
	Idempotency jsontext.Value `json:",omitzero"`
}

// Prerequisites carries Host dependencies; it grants no permission.
type Prerequisites struct {
	Dependencies []ID `json:",omitzero"`
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
	value.Dependencies = slices.Clone(value.Dependencies)

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
	return slices.Equal(a.Dependencies, b.Dependencies)
}

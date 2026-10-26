package sessionstore

import "github.com/unreallabsai/unreal-agent/harness/operation"

// InheritedOperationFacts is absent during ordinary ToolCallStatus handling.
// Its members retain their existing flat canonical JSON names via json/v2 embed.
// Facts are inspection-only; they never confer child Operation ownership.
type InheritedOperationFacts struct {
	InheritedFacts []HistoricalOperation `json:",omitzero"`
}

func (status ToolCallStatus) InheritedFacts() []HistoricalOperation {
	if status.Inheritance == nil {
		return nil
	}
	return status.Inheritance.InheritedFacts
}

// WithInheritedFacts is used while projecting fork history, not on the ordinary
// append path. A new container/slice and detached execution metadata preserve
// the immutable parent's inspection facts without copying receipt bodies.
func WithInheritedFacts(status ToolCallStatus, facts []HistoricalOperation) ToolCallStatus {
	if facts == nil {
		status.Inheritance = nil
		return status
	}
	values := make([]HistoricalOperation, len(facts))
	for i, fact := range facts {
		values[i] = HistoricalOperation(operation.CloneExecutionMetadata(operation.Operation(fact)))
	}
	status.Inheritance = &InheritedOperationFacts{InheritedFacts: values}
	return status
}

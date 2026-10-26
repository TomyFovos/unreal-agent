package operation

import (
	"encoding/json/jsontext"
	"testing"
)

func TestExecutionEvidenceCopiesAndImmutableWorkerUpdates(t *testing.T) {
	original := WithIdempotency(WithPreparationRequired(Operation{Execution: &ExecutionMetadata{ExecutionBasis: jsontext.Value(`{"root":"synthetic"}`), Dependencies: []ID{"prior"}, RecoveryBasis: []UncertainEffect{{ID: "uncertain", Basis: jsontext.Value(`{"version":1}`)}}}}, true), jsontext.Value(`{"key":"original"}`))
	for _, mode := range []string{"own-basis", "blocked-basis", "blocked-id", "dependency", "preparation", "idempotency"} {
		t.Run(mode, func(t *testing.T) {
			worker := CloneExecutionMetadata(original)
			if !SameExecutionMetadata(original, worker) {
				t.Fatal("cloning changed evidence")
			}
			switch mode {
			case "own-basis":
				worker.ExecutionBasis()[2] = 'X'
			case "blocked-basis":
				worker.RecoveryBasis()[0].Basis[2] = 'X'
			case "blocked-id":
				worker.RecoveryBasis()[0].ID = "other"
			case "dependency":
				worker.DependencyIDs()[0] = "other"
			case "preparation":
				worker.Execution.PreparationRequired = false
			case "idempotency":
				worker.Idempotency()[2] = 'X'
			}
			if SameExecutionMetadata(original, worker) {
				t.Fatal("worker changed evidence without detection")
			}
			if string(original.ExecutionBasis()) != `{"root":"synthetic"}` || original.RecoveryBasis()[0].ID != "uncertain" || string(original.RecoveryBasis()[0].Basis) != `{"version":1}` || original.DependencyIDs()[0] != "prior" || !original.RequiresPreparation() || string(original.Idempotency()) != `{"key":"original"}` {
				t.Fatal("worker mutated canonical evidence by aliasing")
			}
		})
	}
}

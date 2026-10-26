package coordinator

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/operation"
)

type canonicalAcknowledgementManager struct {
	*fakeOperationManager
	require bool
	ack     func(operation.Operation) error
}

func (m *canonicalAcknowledgementManager) RequiresExecutionPreparation(operation.Operation) bool {
	return m.require
}

func (m *canonicalAcknowledgementManager) AcknowledgeCanonicalOperation(_ context.Context, value operation.Operation) error {
	return m.ack(value)
}

func TestCanonicalAcknowledgementOnlyAfterSuccessfulTerminalSave(t *testing.T) {
	for _, status := range []operation.Status{operation.StatusReady, operation.StatusAwaiting, operation.StatusCompleted, operation.StatusFailed, operation.StatusCanceled} {
		for _, saveFails := range []bool{false, true} {
			t.Run(string(status)+map[bool]string{false: "/saved", true: "/save_failed"}[saveFails], func(t *testing.T) {
				f := newToolAdmissionFixture(t)
				calls := 0
				f.current.dependencies.Operations = &canonicalAcknowledgementManager{fakeOperationManager: f.manager, ack: func(value operation.Operation) error {
					calls++
					if len(f.store.savedOperations) != 1 || f.store.savedOperations[0].ID != value.ID || !operationIsTerminal(value.Status) {
						t.Fatal("terminal ACK preceded canonical save or changed identity")
					}
					return nil
				}}
				if saveFails {
					f.store.saveOperationErr = errors.New("synthetic save outcome unknown")
				}
				err := f.current.storeOperationInSessionStore(t.Context(), operation.Operation{ID: "synthetic-terminal", Status: status})
				want := 0
				if !saveFails && operationIsTerminal(status) {
					want = 1
				}
				if calls != want || (err != nil) != saveFails {
					t.Fatal("canonical save/ACK order changed")
				}
			})
		}
	}
}

func TestCanonicalAcknowledgementLossDoesNotEraseDurableRecord(t *testing.T) {
	f := newToolAdmissionFixture(t)
	lost := errors.New("synthetic terminal ACK lost")
	f.current.dependencies.Operations = &canonicalAcknowledgementManager{fakeOperationManager: f.manager, ack: func(operation.Operation) error { return lost }}
	err := f.current.storeOperationInSessionStore(t.Context(), operation.Operation{ID: "synthetic-terminal", Status: operation.StatusCompleted})
	if !errors.Is(err, lost) || len(f.store.savedOperations) != 1 || f.store.savedOperations[0].Status != operation.StatusCompleted {
		t.Fatal("ACK loss was treated as a failed canonical save")
	}
}

func TestCanonicalAcknowledgementCannotMutateOwnerEvidence(t *testing.T) {
	f := newToolAdmissionFixture(t)
	op := operation.WithExecutionBasis(operation.Operation{ID: "synthetic-detached-terminal", Status: operation.StatusCompleted, State: jsontext.Value(`{"synthetic":true}`)}, jsontext.Value(`{"synthetic_basis":true}`))
	f.current.dependencies.Operations = &canonicalAcknowledgementManager{fakeOperationManager: f.manager, ack: func(value operation.Operation) error {
		value.State[2] = 'X'
		value.ExecutionBasis()[2] = 'X'
		return nil
	}}
	if err := f.current.storeOperationInSessionStore(t.Context(), op); err != nil {
		t.Fatal(err)
	}
	if string(op.State) != `{"synthetic":true}` || string(op.ExecutionBasis()) != `{"synthetic_basis":true}` {
		t.Fatal("optional backend mutated owner evidence after canonical save")
	}
}

func TestExplicitBackendPreparationDoesNotGrantPermission(t *testing.T) {
	f := newToolAdmissionFixture(t)
	f.current.dependencies.Operations = &canonicalAcknowledgementManager{fakeOperationManager: f.manager, require: true}
	prepared, denial := f.current.prepareRecoveryOperation(t.Context(), operation.Operation{ID: "synthetic-read", ToolName: "read", Status: operation.StatusReady})
	if denial != nil || !prepared.RequiresPreparation() || prepared.Permission != nil || len(prepared.ExecutionBasis()) != 0 || len(f.manager.adds) != 0 {
		t.Fatal("preparation requirement granted execution or permission")
	}
	f.current.dependencies.Operations = f.manager
	ordinary, denial := f.current.prepareRecoveryOperation(t.Context(), operation.Operation{ID: "ordinary-read", ToolName: "read", Status: operation.StatusReady})
	if denial != nil || ordinary.RequiresPreparation() {
		t.Fatal("ordinary backend was changed")
	}
}

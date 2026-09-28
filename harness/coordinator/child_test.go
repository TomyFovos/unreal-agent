package coordinator

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"testing"
)

func TestAcceptedCancelBeatsLateCompletionAndTerminalIsImmutable(t *testing.T) {
	store := emptyFakeStore()
	manager := newFakeOperationManager()
	c := newTestCoordinator(store, newTestInbox(t), manager, contextbuilder.NewBuilder(), tool.NewRegistry(tool.StaticTranslators{}))
	spec, err := operation.NewRemoteJobSpec(operation.RemoteJobPlan{Type: "test", Version: 1, Data: jsontext.Value("{}")})
	if err != nil {
		t.Fatal(err)
	}
	op := operation.Operation{ID: "child", Type: spec.Type, Version: spec.Version, State: spec.State, MaxOutputLength: spec.MaxOutputLength, Status: operation.StatusAwaiting}
	c.state.operations[op.ID] = op
	data, _ := json.Marshal(inbox.ControlMessage{Mode: inbox.CancelOperation, Parameters: inbox.CancelRequest{OperationID: "child"}})
	if err = c.handleInboxInput(t.Context(), inbox.Input{ID: "cancel", Kind: inbox.InputControl, Payload: data}); err != nil {
		t.Fatal(err)
	}
	if len(store.appendedInputs) != 1 || len(store.savedOperations) != 1 || store.savedOperations[0].Status != operation.StatusCanceling || len(manager.cancels) != 1 {
		t.Fatal("cancel was not durably accepted")
	}
	state, _ := operation.DecodeRemoteJobState(op)
	state.TerminalResult = "late result"
	step, _ := operation.UpdateRemoteJob(op, state, operation.StatusCompleted)
	if err = c.handleOperationUpdate(t.Context(), *step.Operation); err != nil {
		t.Fatal(err)
	}
	if c.state.operations[op.ID].Status != operation.StatusCanceled {
		t.Fatal("late completion won")
	}
	count := len(store.savedOperations)
	if err = c.handleOperationUpdate(t.Context(), *step.Operation); err != nil {
		t.Fatal(err)
	}
	if len(store.savedOperations) != count {
		t.Fatal("terminal checkpoint overwritten")
	}
}
func TestCancelStorageFailurePreventsEffect(t *testing.T) {
	run := newStopTestRun(t, 1)
	run.store.saveOperationErr = errors.New("disk")
	run.current.state.operations["operation-0"] = operation.Operation{ID: "operation-0", Status: operation.StatusAwaiting}
	if err := run.current.cancelOperation(t.Context(), "operation-0", "cancel"); err == nil {
		t.Fatal("expected storage error")
	}
	if len(run.operations.cancels) != 0 {
		t.Fatal("effect delivered before checkpoint")
	}
}

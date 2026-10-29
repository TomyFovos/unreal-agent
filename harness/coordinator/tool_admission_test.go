package coordinator

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

// This fixture uses the real Coordinator admission, translation, Operation
// construction, completion and canonical-receipt paths. Only storage and the
// Operation worker are fakes; manager.adds counts dispatches, not side effects.
type toolAdmissionFixture struct {
	current    *coordinator
	store      *fakeStore
	manager    *fakeOperationManager
	translator *submittingTranslator
	registry   tool.Registry
}

func newToolAdmissionFixture(t *testing.T) *toolAdmissionFixture {
	t.Helper()
	spec, err := operation.NewValueSpec(jsontext.Value(`{"synthetic":true}`))
	if err != nil {
		t.Fatal(err)
	}
	f := &toolAdmissionFixture{store: emptyFakeStore(), manager: newFakeOperationManager(), translator: &submittingTranslator{specs: []operation.Spec{spec}}}
	f.registry = tool.NewRegistry(tool.StaticTranslators{Bash: f.translator}, tool.BashName)
	appendHistory := func(kind sessionstore.ItemKind, data any) {
		f.store.items = append(f.store.items, storedItem(sessionstore.Sequence(len(f.store.items)+1), kind, data))
	}
	f.store.onAppendTurn = func(v session.Turn) { appendHistory(sessionstore.ItemTurn, v) }
	f.store.onAppendModelResponse = func(v sessionstore.ModelResponse) {
		// fakeStore invokes this hook even on failure; only successful appends
		// become the durable history used by restore.
		if f.store.appendModelResponseErr == nil {
			appendHistory(sessionstore.ItemModelResponse, v)
		}
	}
	f.store.onAppendToolCallStatus = func(v sessionstore.ToolCallStatus) { appendHistory(sessionstore.ItemToolCallStatus, v) }
	f.current = newTestCoordinator(f.store, newTestInbox(t), f.manager, contextbuilder.NewBuilder(), f.registry)
	f.turn(t, "first")
	return f
}

func (f *toolAdmissionFixture) turn(t *testing.T, id session.TurnID) {
	t.Helper()
	item, err := f.current.addItemToLocalState(sessionstore.Item{Kind: sessionstore.ItemTurn, Data: session.Turn{ID: id, PreviousTurnID: f.current.state.currentTurnID, Type: session.TurnRegular}})
	if err == nil {
		err = f.current.storeItemInSessionStore(t.Context(), item)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func admissionResponse(turn session.TurnID, calls ...llm.ToolCall) sessionstore.ModelResponse {
	response := sessionstore.ModelResponse{TurnID: turn}
	for _, call := range calls {
		response.Response.Output = append(response.Response.Output, llm.Item{Type: llm.ItemToolCall, Data: call})
	}
	return response
}

func admissionCall(id, arguments string) llm.ToolCall {
	return llm.ToolCall{CallID: id, Name: tool.BashName, Arguments: arguments}
}

func (f *toolAdmissionFixture) acceptAndComplete(t *testing.T, call llm.ToolCall) {
	t.Helper()
	before := len(f.manager.adds)
	if _, err := f.current.handleModelResponse(t.Context(), admissionResponse(f.current.state.currentTurnID, call)); err != nil {
		t.Fatal(err)
	}
	if err := f.current.dispatchOperationsToManager(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(f.manager.adds) != before+1 {
		t.Fatalf("new dispatches = %d, want 1", len(f.manager.adds)-before)
	}
	completed := f.manager.adds[before]
	completed.Status = operation.StatusCompleted
	if err := f.current.handleOperationUpdate(t.Context(), completed); err != nil {
		t.Fatal(err)
	}
	if _, err := f.current.reconcileToolCalls(t.Context()); err != nil {
		t.Fatal(err)
	}
	statuses := f.store.appendedStatuses
	if len(statuses) == 0 || len(statuses[len(statuses)-1].Operations) != 1 || statuses[len(statuses)-1].Operations[0].ID != completed.ID || statuses[len(statuses)-1].Operations[0].Status != operation.StatusCompleted {
		t.Fatal("completed Operation did not produce the corresponding canonical receipt")
	}
	t.Logf("call_id=%s operation_id=%s dispatches=%d canonical_receipts=%d", call.CallID, completed.ID, len(f.manager.adds), len(statuses))
}

func (f *toolAdmissionFixture) restart(t *testing.T) {
	t.Helper()
	f.manager = newFakeOperationManager()
	f.current = newTestCoordinator(f.store, newTestInbox(t), f.manager, contextbuilder.NewBuilder(), f.registry)
	if err := f.current.restore(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestToolAdmissionAppendFailureDoesNotAdmitOrCreateOperation(t *testing.T) {
	f := newToolAdmissionFixture(t)
	want := errors.New("synthetic-response-append-failure")
	f.store.appendModelResponseErr = want
	response := admissionResponse("first", admissionCall("after-failed-append", `{}`))
	before := len(f.store.items)
	if _, err := f.current.handleModelResponse(t.Context(), response); !errors.Is(err, want) {
		t.Fatalf("append error = %v", err)
	}
	if len(f.store.items) != before || len(f.translator.calls) != 0 || len(f.current.state.operations) != 0 || len(f.manager.adds) != 0 {
		t.Fatal("failed append created canonical or executable state")
	}
	f.store.appendModelResponseErr = nil
	f.acceptAndComplete(t, response.Response.Output[0].Data.(llm.ToolCall))
}

// The storage wrapper injects a crash-shaped failure after a successful durable
// append. Recovery must read that canonical response, rather than propose it
// again through the new-response admission boundary.
type admissionCommittedErrorStore struct {
	sessionstore.Store
	err error
}

func (s *admissionCommittedErrorStore) AppendModelResponse(ctx context.Context, id session.ID, response sessionstore.ModelResponse) error {
	if err := s.Store.AppendModelResponse(ctx, id, response); err != nil {
		return err
	}
	return s.err
}

func TestToolAdmissionCommittedUnexecutedResponseRestoresOnce(t *testing.T) {
	f := newToolAdmissionFixture(t)
	want := errors.New("synthetic-after-durable-append")
	f.current.dependencies.Sessions = &admissionCommittedErrorStore{Store: f.store, err: want}
	call := admissionCall("durable-before-operation", `{}`)
	response := admissionResponse("first", call)
	if _, err := f.current.handleModelResponse(t.Context(), response); !errors.Is(err, want) {
		t.Fatalf("post-append failure = %v", err)
	}
	if len(f.store.appendedResponses) != 1 || len(f.translator.calls) != 0 || len(f.manager.adds) != 0 {
		t.Fatal("post-append failure dispatched or lost the response")
	}
	f.restart(t)
	if _, err := f.current.scheduleToolCalls(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := f.current.dispatchOperationsToManager(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(f.manager.adds) != 1 || len(f.translator.calls) != 1 {
		t.Fatalf("restored committed call: dispatches=%d translations=%d, want one each", len(f.manager.adds), len(f.translator.calls))
	}
}

func TestToolAdmissionNormalResponsesDoNotRereadHistory(t *testing.T) {
	f := newToolAdmissionFixture(t)
	for i := range 20 {
		f.acceptAndComplete(t, admissionCall(fmt.Sprintf("incremental-%d", i), `{}`))
		f.turn(t, session.TurnID(fmt.Sprintf("turn-%d", i)))
	}
	if len(f.store.itemRequests) != 0 {
		t.Fatalf("ordinary responses reread canonical history %d times", len(f.store.itemRequests))
	}
}

func TestToolAdmissionRejectsFinalHistoryCursorMismatch(t *testing.T) {
	f := newToolAdmissionFixture(t)
	f.store.itemsPage = &sessionstore.Page{Items: []sessionstore.Item{storedItem(1, sessionstore.ItemTurn, session.Turn{ID: "first", Type: session.TurnRegular})}, NextAfter: 2}
	if err := f.current.restore(t.Context()); !errors.Is(err, llm.ErrToolRecoveryRequired) {
		t.Fatalf("incomplete final page error = %v, want RecoveryRequired", err)
	}
	if len(f.manager.adds) != 0 || len(f.translator.calls) != 0 {
		t.Fatal("inconsistent history produced executable work")
	}
}

func TestToolAdmissionWholeValidBatchIsDurableBeforeTranslation(t *testing.T) {
	f := newToolAdmissionFixture(t)
	first, second := admissionCall("batch-first", `{}`), admissionCall("batch-second", `{"second":true}`)
	response := admissionResponse("first", first, second)
	f.translator.onTranslate = func() {
		if len(f.store.appendedResponses) != 1 || len(f.store.appendedResponses[0].Response.Output) != 2 {
			t.Error("batch translated before its complete ModelResponse became durable")
		}
	}
	statuses, err := f.current.handleModelResponse(t.Context(), response)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 2 || len(f.translator.calls) != 2 || len(f.current.state.operations) != 2 || len(f.manager.adds) != 0 {
		t.Fatal("valid batch did not construct exactly two Operations after durable admission")
	}
	for _, status := range statuses {
		if len(status.Operations) != 1 || status.Operations[0].ID == "" || (status.CallID != first.CallID && status.CallID != second.CallID) {
			t.Fatal("accepted batch lost Tool Call/Operation identity")
		}
	}
	if statuses[0].Operations[0].ID == statuses[1].Operations[0].ID {
		t.Fatal("two accepted calls share an Operation ID")
	}
}

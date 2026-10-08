package coordinator

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

func TestToolBridgeContinuationWatermarkPreservesQueuedInputOnReplay(t *testing.T) {
	store := emptyFakeStore()
	appendHistory := func(kind sessionstore.ItemKind, data any) {
		store.items = append(store.items, storedItem(sessionstore.Sequence(len(store.items)+1), kind, data))
	}
	store.onAppendInput = func(v inbox.Input) { appendHistory(sessionstore.ItemInput, v) }
	store.onAppendTurn = func(v session.Turn) { appendHistory(sessionstore.ItemTurn, v) }
	store.onAppendModelResponse = func(v sessionstore.ModelResponse) { appendHistory(sessionstore.ItemModelResponse, v) }
	store.onAppendToolCallStatus = func(v sessionstore.ToolCallStatus) { appendHistory(sessionstore.ItemToolCallStatus, v) }
	spec, err := operation.NewValueSpec(jsontext.Value(`{"fixture":true}`))
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry(tool.StaticTranslators{Bash: &submittingTranslator{specs: []operation.Spec{spec}}}, tool.BashName)
	manager := newFakeOperationManager()
	current := newTestCoordinator(store, newTestInbox(t), manager, contextbuilder.NewBuilder(), registry)
	commit := func(kind sessionstore.ItemKind, data any) {
		t.Helper()
		i, err := current.addItemToLocalState(sessionstore.Item{Kind: kind, Data: data})
		if err == nil {
			err = current.storeItemInSessionStore(t.Context(), i)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	commit(sessionstore.ItemInput, externalEvent(t, 0, "first", "first task"))
	commit(sessionstore.ItemTurn, session.Turn{ID: "origin", Type: session.TurnRegular, RuntimeRevision: 7})
	current.cancelModel = func() {}
	current.modelOrigin, current.modelRevision, current.modelInputs = "origin", 7, 1
	request := bridgeRequest{ctx: t.Context(), origin: "origin", reply: make(chan bridgeReply, 1), response: llm.Response{ID: "public-round", Output: []llm.Item{{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "call-1", Name: tool.BashName, Arguments: `{}`}}}}}
	if err := current.acceptBridgeRequest(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if len(manager.adds) != 1 || len(store.appendedResponses) != 1 || len(store.appendedStatuses) != 1 {
		t.Fatal("tool callback bypassed durable response/Operation creation")
	}
	commit(sessionstore.ItemInput, externalEvent(t, 0, "queued", "queued while Claude waits for a receipt"))
	completed := manager.adds[0]
	completed.Status = operation.StatusCompleted
	if err := current.handleOperationUpdate(t.Context(), completed); err != nil {
		t.Fatal(err)
	}
	if callModel, err := current.processEvents(t.Context()); err != nil || callModel {
		t.Fatal("receipt restarted the still-running provider process", err)
	}
	reply := <-request.reply
	if reply.err != nil || len(reply.outcomes) != 1 || reply.outcomes[0].Failed {
		t.Fatal("terminal receipt not delivered", reply.err)
	}
	refresh := bridgeRequest{ctx: t.Context(), origin: "origin", reply: make(chan bridgeReply, 1), refresh: true, reserve: 1024}
	if err := current.acceptBridgeRequest(t.Context(), refresh); err != nil {
		t.Fatal(err)
	}
	refreshed := <-refresh.reply
	data, _ := json.Marshal(refreshed.request)
	if refreshed.err != nil || strings.Contains(string(data), "queued while Claude") || !strings.Contains(string(data), "first task") {
		t.Fatal("queued input leaked into a pinned tool generation", refreshed.err)
	}
	if current.state.currentTurnInputs != 2 || current.state.availableInputs != 3 || len(store.appendedTurns) != 2 {
		t.Fatal("queued user input was incorrectly acknowledged by the tool result", current.state.currentTurnInputs, current.state.availableInputs)
	}
	turn := store.appendedTurns[1]
	if turn.ToolContinuation != "origin" || turn.RuntimeRevision != 7 || turn.InputWatermark != 2 {
		t.Fatal("continuation changed canonical runtime or delivery boundary", turn)
	}
	commit(sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: turn.ID, Response: textResponse("final response")})
	if current.pendingInputs() != 1 {
		t.Fatal("queued input lost after final response")
	}
	replayed := newTestCoordinator(store, newTestInbox(t), newFakeOperationManager(), contextbuilder.NewBuilder(), registry)
	if err := replayed.restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	if replayed.pendingInputs() != 1 || len(replayed.state.toolCalls) != 0 || replayed.state.currentTurnID != turn.ID {
		t.Fatal("resume lost queued input or replayed an already completed Operation")
	}
}

func TestStructuredCanonicalReceiptReplayAndUncertainOperation(t *testing.T) {
	store := emptyFakeStore()
	manager := newFakeOperationManager()
	spec, err := operation.NewValueSpec(jsontext.Value(`{"fixture":true}`))
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry(tool.StaticTranslators{Bash: &submittingTranslator{specs: []operation.Spec{spec}}}, tool.BashName)
	current := newTestCoordinator(store, newTestInbox(t), manager, contextbuilder.NewBuilder(), registry)
	current.state.currentTurnID = "origin"
	current.cancelModel = func() {}
	current.modelOrigin = "origin"
	call := llm.ToolCall{CallID: "structured-canonical-input-action", Name: tool.BashName, Arguments: `{}`}
	request := func(call llm.ToolCall) bridgeRequest {
		return bridgeRequest{ctx: t.Context(), origin: current.modelOrigin, reply: make(chan bridgeReply, 1), response: llm.Response{Output: []llm.Item{{Type: llm.ItemToolCall, Data: call}}}}
	}
	first := request(call)
	if err := current.acceptBridgeRequest(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	completed := manager.adds[0]
	completed.Status = operation.StatusCompleted
	if err := current.handleOperationUpdate(t.Context(), completed); err != nil {
		t.Fatal(err)
	}
	if _, err := current.processEvents(t.Context()); err != nil {
		t.Fatal(err)
	}
	outcomes := (<-first.reply).outcomes
	// Simulate restart by rebuilding the disposable replay cache from canonical
	// records, exactly as restoreItem does; no provider-private action state.
	rebuilt := newTestCoordinator(emptyFakeStore(), newTestInbox(t), newFakeOperationManager(), contextbuilder.NewBuilder(), registry)
	rebuilt.cancelModel = func() {}
	rebuilt.modelOrigin = "resumed"
	rebuilt.addToolCallsToLocalState(sessionstore.ModelResponse{TurnID: "origin", Response: first.response})
	status := sessionstore.ToolCallStatus{TurnID: "origin", CallID: call.CallID, Status: tool.CallStatus{}, Operations: []operation.Operation{completed}}
	rebuilt.rememberBridgeResult(status, outcomes[0].Result, nil)
	current = rebuilt
	replay := request(call)
	if err := current.acceptBridgeRequest(t.Context(), replay); err != nil {
		t.Fatal(err)
	}
	result := <-replay.reply
	if result.err != nil || len(result.outcomes) != 1 || len(rebuilt.dependencies.Operations.(*fakeOperationManager).adds) != 0 || len(rebuilt.dependencies.Sessions.(*fakeStore).appendedResponses) != 0 {
		t.Fatal("completed canonical action executed twice", result.err)
	}
	conflict := call
	conflict.Arguments = `{"changed":true}`
	bad := request(conflict)
	if err := current.acceptBridgeRequest(t.Context(), bad); err != nil {
		t.Fatal(err)
	}
	if !errors.Is((<-bad.reply).err, llm.ErrToolIdentityConflict) {
		t.Fatal("identity collision accepted")
	}
	current.structuredCalls[call.CallID].outcome = nil
	uncertain := request(call)
	if err := current.acceptBridgeRequest(t.Context(), uncertain); err != nil {
		t.Fatal(err)
	}
	if !errors.Is((<-uncertain.reply).err, llm.ErrToolRecoveryRequired) {
		t.Fatal("uncertain operation repeated")
	}
	if _, err := current.addItemToLocalState(sessionstore.Item{Kind: sessionstore.ItemFork, Data: sessionstore.Fork{ParentID: "parent", PreviousTurnID: "origin"}}); err != nil || len(current.structuredCalls) != 0 {
		t.Fatal("fork retained parent execution ownership", err)
	}
}

func TestToolBridgeInvalidPrivateOutputCannotCreateOperation(t *testing.T) {
	for _, output := range []llm.Item{
		{Type: llm.ItemReasoning, Data: llm.Reasoning{}},
		{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleSystem, Text: "private"}},
	} {
		store := emptyFakeStore()
		manager := newFakeOperationManager()
		current := newTestCoordinator(store, newTestInbox(t), manager, contextbuilder.NewBuilder(), tool.NewRegistry(tool.StaticTranslators{}))
		current.cancelModel = func() {}
		current.modelOrigin = "origin"
		request := bridgeRequest{ctx: context.Background(), origin: "origin", reply: make(chan bridgeReply, 1), response: llm.Response{Output: []llm.Item{output, {Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "call", Name: "read", Arguments: `{}`}}}}}
		if err := current.acceptBridgeRequest(t.Context(), request); err == nil || len(store.appendedResponses) != 0 || len(manager.adds) != 0 {
			t.Fatal("private callback output reached canonical history or execution")
		}
	}
}

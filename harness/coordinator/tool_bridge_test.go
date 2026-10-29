package coordinator

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"slices"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

func TestSteeringContinuationWatermarkSurvivesReplayAndKeepsPeerPending(t *testing.T) {
	store := emptyFakeStore()
	appendItem := func(kind sessionstore.ItemKind, data any) {
		store.items = append(store.items, storedItem(sessionstore.Sequence(len(store.items)+1), kind, data))
	}
	store.onAppendInput = func(in inbox.Input) { appendItem(sessionstore.ItemInput, in) }
	store.onAppendTurn = func(turn session.Turn) { appendItem(sessionstore.ItemTurn, turn) }
	store.onAppendModelResponse = func(response sessionstore.ModelResponse) { appendItem(sessionstore.ItemModelResponse, response) }
	c := newTestCoordinator(store, newTestInbox(t), newFakeOperationManager(), contextbuilder.NewBuilder(), tool.NewRegistry(tool.StaticTranslators{}))
	commit := func(kind sessionstore.ItemKind, data any) {
		t.Helper()
		item, err := c.addItemToLocalState(sessionstore.Item{Kind: kind, Data: data})
		if err == nil {
			err = c.storeItemInSessionStore(t.Context(), item)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	commit(sessionstore.ItemInput, externalEvent(t, 0, "task", "work"))
	commit(sessionstore.ItemTurn, session.Turn{ID: "origin", Type: session.TurnRegular})
	c.cancelModel = func() {}
	c.bridgeActive = true
	c.modelOrigin = "origin"
	c.modelInputs = 1
	c.turnExternals = 1
	commit(sessionstore.ItemInput, externalEvent(t, 1, "first", "answer first"))
	peer, _ := json.Marshal(inbox.PeerMessage{Version: 1, Sender: "child", Target: "session-1", Handle: "h", Kind: "ended", Text: "child ended"})
	commit(sessionstore.ItemInput, inbox.Input{ID: "peer", Kind: inbox.InputPeer, Payload: peer})
	commit(sessionstore.ItemInput, externalEvent(t, 2, "second", "answer second"))
	r := bridgeRequest{ctx: t.Context(), origin: "origin", reply: make(chan bridgeReply, 1), steering: true}
	if err := c.acceptBridgeRequest(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	reply := <-r.reply
	if !slices.Equal(reply.inputs, []string{"answer first", "answer second"}) || c.state.currentTurnInputs != 3 || len(c.deferredContext) != 1 {
		t.Fatalf("delivery=%+v watermark=%d deferred=%d", reply, c.state.currentTurnInputs, len(c.deferredContext))
	}
	commit(sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: c.state.currentTurnID, Response: textResponse("answered")})
	if c.pendingInputs() != 1 {
		t.Fatal("peer incorrectly consumed", c.pendingInputs())
	}
	replayed := newTestCoordinator(store, newTestInbox(t), newFakeOperationManager(), contextbuilder.NewBuilder(), tool.NewRegistry(tool.StaticTranslators{}))
	if err := replayed.restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	if replayed.pendingInputs() != 1 || replayed.state.currentTurnInputs != 3 || replayed.turnExternals != 3 {
		t.Fatal("replay lost delivery watermark", replayed.pendingInputs(), replayed.state.currentTurnInputs)
	}
}

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

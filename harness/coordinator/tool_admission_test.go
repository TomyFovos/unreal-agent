package coordinator

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"strings"
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

func (f *toolAdmissionFixture) reject(t *testing.T, response sessionstore.ModelResponse, want error) {
	t.Helper()
	responses, statuses, translated, dispatched := len(f.store.appendedResponses), len(f.store.appendedStatuses), len(f.translator.calls), len(f.manager.adds)
	operations, pendingCalls := len(f.current.state.operations), len(f.current.state.toolCalls)
	_, err := f.current.handleModelResponse(t.Context(), response)
	if err == nil || (want != nil && !errors.Is(err, want)) {
		t.Errorf("admission error = %v, want %v", err, want)
	}
	if len(f.store.appendedResponses) != responses || len(f.store.appendedStatuses) != statuses || len(f.translator.calls) != translated || len(f.manager.adds) != dispatched {
		t.Errorf("rejected response changed canonical/translation/dispatch state: responses %d->%d statuses %d->%d translations %d->%d dispatches %d->%d", responses, len(f.store.appendedResponses), statuses, len(f.store.appendedStatuses), translated, len(f.translator.calls), dispatched, len(f.manager.adds))
	}
	if len(f.current.state.operations) != operations || len(f.current.state.toolCalls) != pendingCalls {
		t.Error("rejected response generated executable local state")
	}
}

func TestToolAdmissionKnownFailures(t *testing.T) {
	for _, name := range []string{"repeat", "collision", "new-turn", "restart", "projection-drop"} {
		t.Run(name, func(t *testing.T) {
			f := newToolAdmissionFixture(t)
			call := admissionCall("synthetic-original-id", `{"path":"synthetic.txt"}`)
			f.acceptAndComplete(t, call)
			want := llm.ErrToolRecoveryRequired
			switch name {
			case "collision":
				call.Arguments = `{"path":"different.txt"}`
				want = llm.ErrToolIdentityConflict
			case "new-turn":
				f.turn(t, "second")
			case "restart":
				f.restart(t)
				f.turn(t, "resumed")
			case "projection-drop":
				// The provider-facing projection deliberately has no old call;
				// canonical history and admission ownership are unchanged.
				f.current.dependencies.ContextBuilder = contextbuilder.NewBuilder()
				f.turn(t, "new-projection")
				built, err := f.current.dependencies.ContextBuilder.Build()
				if err != nil {
					t.Fatal(err)
				}
				for _, item := range built.Request.Input {
					if item.Type == llm.ItemToolCall {
						t.Fatal("fixture retained an old Tool Call in the projection")
					}
				}
			}
			f.reject(t, admissionResponse(f.current.state.currentTurnID, call), want)
		})
	}
}

func TestToolAdmissionWholeResponseBeforeAnyExecution(t *testing.T) {
	for _, name := range []string{"same-id", "same-id-different-payload", "existing-second", "malformed-second"} {
		t.Run(name, func(t *testing.T) {
			f := newToolAdmissionFixture(t)
			first, second := admissionCall("new", `{}`), admissionCall("new", `{}`)
			var want error
			switch name {
			case "same-id":
				want = llm.ErrToolRecoveryRequired
			case "same-id-different-payload":
				second.Arguments = `{"different":true}`
				want = llm.ErrToolIdentityConflict
			case "existing-second":
				second.CallID = "old"
				f.acceptAndComplete(t, second)
				want = llm.ErrToolRecoveryRequired
			case "malformed-second":
				second.CallID, second.Arguments = "malformed", `{"unfinished":`
			}
			f.reject(t, admissionResponse(f.current.state.currentTurnID, first, second), want)
		})
	}
}

func TestToolAdmissionCanonicalJSONIdentity(t *testing.T) {
	for _, test := range []struct {
		name, first, second string
		want                error
	}{
		{"keys-and-whitespace", `{"a":1,"b":{"c":2,"d":[true,false]}}`, " { \"b\" : {\"d\":[true, false],\"c\":2}, \"a\":1 } ", llm.ErrToolRecoveryRequired},
		{"large-integer-equal", `{"n":9007199254740993,"m":1}`, `{"m":1,"n":9007199254740993}`, llm.ErrToolRecoveryRequired},
		{"large-integer-different", `{"n":9007199254740992}`, `{"n":9007199254740993}`, llm.ErrToolIdentityConflict},
		{"nested-value-different", `{"a":{"b":[1,2]}}`, `{"a":{"b":[2,1]}}`, llm.ErrToolIdentityConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newToolAdmissionFixture(t)
			f.acceptAndComplete(t, admissionCall("id", test.first))
			f.reject(t, admissionResponse(f.current.state.currentTurnID, admissionCall("id", test.second)), test.want)
		})
	}
}

func TestToolAdmissionMalformedIdentityNeverAppends(t *testing.T) {
	for _, test := range []struct{ name, id, tool, args string }{
		{"empty-id", "", tool.BashName, `{}`},
		{"empty-tool", "id", "", `{}`},
		{"empty-arguments", "id", tool.BashName, ""},
		{"truncated", "id", tool.BashName, `{"a":`},
		{"trailing-json", "id", tool.BashName, `{} {}`},
		{"duplicate-key", "id", tool.BashName, `{"a":1,"a":2}`},
		{"null", "id", tool.BashName, `null`},
		{"array", "id", tool.BashName, `[]`},
		{"scalar", "id", tool.BashName, `1`},
		{"non-json-whitespace", "id", tool.BashName, "\u00a0{}"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newToolAdmissionFixture(t)
			f.reject(t, admissionResponse("first", llm.ToolCall{CallID: test.id, Name: test.tool, Arguments: test.args}), nil)
		})
	}
}

func TestToolAdmissionNameConflictAndIndependentSessions(t *testing.T) {
	t.Run("name-conflict", func(t *testing.T) {
		f := newToolAdmissionFixture(t)
		call := admissionCall("id", `{}`)
		f.acceptAndComplete(t, call)
		call.Name = tool.ReadName
		f.reject(t, admissionResponse("first", call), llm.ErrToolIdentityConflict)
	})
	t.Run("independent-session", func(t *testing.T) {
		first, second := newToolAdmissionFixture(t), newToolAdmissionFixture(t)
		second.current.dependencies.SessionID = "independent-session"
		call := admissionCall("same-id-different-session", `{}`)
		first.acceptAndComplete(t, call)
		second.acceptAndComplete(t, call)
		if first.manager.adds[0].ID == second.manager.adds[0].ID {
			t.Fatal("independent Sessions shared an Operation identity")
		}
	})
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
	// The owner cannot know whether an arbitrary failed append committed. It
	// must defer further admission to canonical restore, even though this
	// synthetic fault is known not to have committed.
	f.reject(t, response, llm.ErrToolRecoveryRequired)
	f.restart(t)
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
	f.reject(t, response, llm.ErrToolRecoveryRequired)
}

func TestToolAdmissionForkRetainsIdentityWithoutParentExecutionAuthority(t *testing.T) {
	for _, completed := range []bool{false, true} {
		t.Run(fmt.Sprintf("parent-completed=%t", completed), func(t *testing.T) {
			f := newToolAdmissionFixture(t)
			call := admissionCall("inherited-id", `{}`)
			if completed {
				f.acceptAndComplete(t, call)
			} else if _, err := f.current.handleModelResponse(t.Context(), admissionResponse("first", call)); err != nil {
				t.Fatal(err)
			}
			f.store.items = append(f.store.items, storedItem(sessionstore.Sequence(len(f.store.items)+1), sessionstore.ItemFork, sessionstore.Fork{ParentID: "parent", PreviousTurnID: "first"}))
			f.restart(t)
			f.current.dependencies.SessionID = "child"
			if len(f.current.state.toolCalls) != 0 || len(f.current.state.operations) != 0 || len(f.manager.adds) != 0 {
				t.Fatal("fork inherited executable parent work")
			}
			f.turn(t, "child-turn")
			f.reject(t, admissionResponse("child-turn", call), llm.ErrToolRecoveryRequired)
			f.acceptAndComplete(t, admissionCall("fresh-child-id", `{}`))
		})
	}
}

func TestToolAdmissionMalformedCanonicalHistoryFailsClosed(t *testing.T) {
	for _, name := range []string{"malformed-arguments", "duplicate-response", "identity-collision", "sequence-gap", "tool-data-type"} {
		t.Run(name, func(t *testing.T) {
			f := newToolAdmissionFixture(t)
			call := admissionCall("canonical-id", `{}`)
			response := admissionResponse("first", call)
			f.store.items = append(f.store.items, storedItem(2, sessionstore.ItemModelResponse, response))
			switch name {
			case "malformed-arguments":
				call.Arguments = `{"secret-canary":`
				f.store.items[1].Data = admissionResponse("first", call)
			case "duplicate-response", "identity-collision":
				if name == "identity-collision" {
					call.Arguments = `{"changed":true}`
				}
				f.store.items = append(f.store.items, storedItem(3, sessionstore.ItemModelResponse, admissionResponse("second", call)))
			case "sequence-gap":
				f.store.items[1].Sequence = 3
			case "tool-data-type":
				response.Response.Output[0].Data = llm.Message{Role: llm.RoleAssistant, Text: "secret-canary"}
				f.store.items[1].Data = response
			}
			current := newTestCoordinator(f.store, newTestInbox(t), newFakeOperationManager(), contextbuilder.NewBuilder(), f.registry)
			err := current.restore(t.Context())
			if err == nil {
				t.Fatal("malformed canonical history restored without fail-closed error")
			}
			if strings.Contains(err.Error(), "secret-canary") || len(f.translator.calls) != 0 {
				t.Fatal("restore leaked arguments or translated untrusted history")
			}
		})
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

func TestToolAdmissionBridgeUsesSessionWideBoundary(t *testing.T) {
	for _, name := range []string{"ordinary-existing-id", "new-first-existing-second", "duplicate-in-callback", "conflict-in-callback"} {
		t.Run(name, func(t *testing.T) {
			f := newToolAdmissionFixture(t)
			old := admissionCall("ordinary-id", `{}`)
			f.acceptAndComplete(t, old)
			f.current.cancelModel = func() {}
			f.current.modelOrigin = "first"
			calls := []llm.ToolCall{old}
			want := llm.ErrToolRecoveryRequired
			switch name {
			case "new-first-existing-second":
				calls = []llm.ToolCall{admissionCall("new-callback-id", `{}`), old}
			case "duplicate-in-callback", "conflict-in-callback":
				fresh := admissionCall("duplicate-callback-id", `{}`)
				other := fresh
				if name == "conflict-in-callback" {
					other.Arguments = `{"different":true}`
					want = llm.ErrToolIdentityConflict
				}
				calls = []llm.ToolCall{fresh, other}
			}
			responses, statuses, translated, dispatched := len(f.store.appendedResponses), len(f.store.appendedStatuses), len(f.translator.calls), len(f.manager.adds)
			request := bridgeRequest{ctx: t.Context(), origin: "first", response: admissionResponse("first", calls...).Response, reply: make(chan bridgeReply, 1)}
			err := f.current.acceptBridgeRequest(t.Context(), request)
			if err == nil {
				select {
				case reply := <-request.reply:
					err = reply.err
				default:
					t.Fatal("rejected callback remained pending")
				}
			}
			if !errors.Is(err, want) {
				t.Errorf("callback error = %v, want %v", err, want)
			}
			if len(f.store.appendedResponses) != responses || len(f.store.appendedStatuses) != statuses || len(f.translator.calls) != translated || len(f.manager.adds) != dispatched {
				t.Fatal("rejected callback created new canonical or executable work")
			}
		})
	}
}

func TestToolAdmissionCanonicalStatusRequiresOwnedIdentity(t *testing.T) {
	for _, name := range []string{"orphan-status", "mismatched-status-turn", "status-before-response"} {
		t.Run(name, func(t *testing.T) {
			f := newToolAdmissionFixture(t)
			call := admissionCall("owned-id", `{}`)
			status := sessionstore.ToolCallStatus{TurnID: "first", CallID: call.CallID, Status: tool.CallStatus{Error: "synthetic-denial"}}
			response := admissionResponse("first", call)
			switch name {
			case "orphan-status":
				f.store.items = append(f.store.items, storedItem(2, sessionstore.ItemToolCallStatus, status))
			case "mismatched-status-turn":
				status.TurnID = "unrelated-turn"
				f.store.items = append(f.store.items, storedItem(2, sessionstore.ItemModelResponse, response), storedItem(3, sessionstore.ItemToolCallStatus, status))
			case "status-before-response":
				f.store.items = append(f.store.items, storedItem(2, sessionstore.ItemToolCallStatus, status), storedItem(3, sessionstore.ItemModelResponse, response))
			}
			manager := newFakeOperationManager()
			current := newTestCoordinator(f.store, newTestInbox(t), manager, contextbuilder.NewBuilder(), f.registry)
			if err := current.restore(t.Context()); !errors.Is(err, llm.ErrToolRecoveryRequired) {
				t.Fatalf("status without previously admitted Turn/Call identity: %v", err)
			}
			if len(manager.adds) != 0 || len(f.translator.calls) != 0 || len(f.store.appendedResponses) != 0 || len(f.store.appendedStatuses) != 0 {
				t.Fatal("invalid status history translated, executed or rewrote canonical state")
			}
		})
	}
}

func TestToolAdmissionCompactionCallsReserveIdentityWithoutExecution(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprintf("restart=%t", restart), func(t *testing.T) {
			f := newToolAdmissionFixture(t)
			item := sessionstore.Item{Kind: sessionstore.ItemTurn, Data: session.Turn{ID: "compact", PreviousTurnID: "first", Type: session.TurnCompaction}}
			if _, err := f.current.addItemToLocalState(item); err != nil {
				t.Fatal(err)
			}
			if err := f.current.storeItemInSessionStore(t.Context(), item); err != nil {
				t.Fatal(err)
			}
			call := admissionCall("compaction-only-id", `{"synthetic":true}`)
			statuses, err := f.current.handleModelResponse(t.Context(), admissionResponse("compact", call))
			if err != nil {
				t.Fatal(err)
			}
			if len(statuses) != 0 || len(f.store.appendedResponses) != 1 || len(f.store.appendedStatuses) != 0 || len(f.current.state.toolCalls) != 0 || len(f.current.state.operations) != 0 || len(f.translator.calls) != 0 || len(f.manager.adds) != 0 {
				t.Fatal("compaction Tool Call was not durably inert")
			}
			if restart {
				f.restart(t)
				if len(f.current.state.toolCalls) != 0 || len(f.current.state.operations) != 0 || len(f.manager.adds) != 0 {
					t.Fatal("restoring compaction made its Tool Call executable")
				}
			}
			f.turn(t, "after-compaction")
			f.reject(t, admissionResponse("after-compaction", call), llm.ErrToolRecoveryRequired)
		})
	}
}

func TestToolAdmissionStructuredReceiptReplayUsesCanonicalArguments(t *testing.T) {
	for _, name := range []string{"equivalent-json", "different-name", "different-arguments"} {
		t.Run(name, func(t *testing.T) {
			f := newToolAdmissionFixture(t)
			call := admissionCall("structured-synthetic-callback", `{"path":"synthetic.txt","nested":{"a":1,"b":9007199254740993}}`)
			f.acceptAndComplete(t, call)
			f.current.cancelModel = func() {}
			f.current.modelOrigin = "first"
			responses, statuses, translated, dispatched := len(f.store.appendedResponses), len(f.store.appendedStatuses), len(f.translator.calls), len(f.manager.adds)
			var want error
			switch name {
			case "equivalent-json":
				call.Arguments = " { \"nested\" : { \"b\":9007199254740993, \"a\" : 1 }, \"path\":\"synthetic.txt\" } "
			case "different-name":
				call.Name = tool.ReadName
				want = llm.ErrToolIdentityConflict
			case "different-arguments":
				call.Arguments = `{"path":"synthetic.txt","nested":{"a":1,"b":9007199254740992}}`
				want = llm.ErrToolIdentityConflict
			}
			request := bridgeRequest{ctx: t.Context(), origin: "first", response: admissionResponse("first", call).Response, reply: make(chan bridgeReply, 1)}
			if err := f.current.acceptBridgeRequest(t.Context(), request); err != nil {
				t.Fatal(err)
			}
			var reply bridgeReply
			select {
			case reply = <-request.reply:
			default:
				t.Fatal("canonical receipt replay remained pending")
			}
			if want == nil {
				if reply.err != nil || len(reply.outcomes) != 1 || reply.outcomes[0].Result.CallID != call.CallID || reply.outcomes[0].Failed {
					t.Fatalf("equivalent arguments did not reuse the terminal receipt: %v", reply.err)
				}
			} else if !errors.Is(reply.err, want) || len(reply.outcomes) != 0 {
				t.Fatalf("conflicting callback returned receipt: %v", reply.err)
			}
			if len(f.store.appendedResponses) != responses || len(f.store.appendedStatuses) != statuses || len(f.translator.calls) != translated || len(f.manager.adds) != dispatched {
				t.Fatal("receipt replay or identity conflict created additional work")
			}
		})
	}
}

func TestToolAdmissionTaintedAppendCannotUseBridgeCallback(t *testing.T) {
	for _, name := range []string{"fresh-call", "existing-receipt"} {
		t.Run(name, func(t *testing.T) {
			f := newToolAdmissionFixture(t)
			old := admissionCall("structured-terminal-before-taint", `{}`)
			f.acceptAndComplete(t, old)
			want := errors.New("synthetic-unknown-append-outcome")
			f.store.appendModelResponseErr = want
			if _, err := f.current.handleModelResponse(t.Context(), admissionResponse("first", admissionCall("uncertain-append", `{}`))); !errors.Is(err, want) {
				t.Fatalf("failure injection did not reach append boundary: %v", err)
			}
			f.store.appendModelResponseErr = nil
			f.current.cancelModel = func() {}
			f.current.modelOrigin = "first"
			responses, statuses, translated, dispatched := len(f.store.appendedResponses), len(f.store.appendedStatuses), len(f.translator.calls), len(f.manager.adds)
			call := admissionCall("fresh-after-taint", `{}`)
			if name == "existing-receipt" {
				call = old
			}
			request := bridgeRequest{ctx: t.Context(), origin: "first", response: admissionResponse("first", call).Response, reply: make(chan bridgeReply, 1)}
			if err := f.current.acceptBridgeRequest(t.Context(), request); err != nil {
				t.Fatal(err)
			}
			select {
			case reply := <-request.reply:
				if !errors.Is(reply.err, llm.ErrToolRecoveryRequired) || len(reply.outcomes) != 0 {
					t.Fatalf("tainted owner processed callback: %v", reply.err)
				}
			default:
				t.Fatal("tainted callback remained pending")
			}
			if len(f.store.appendedResponses) != responses || len(f.store.appendedStatuses) != statuses || len(f.translator.calls) != translated || len(f.manager.adds) != dispatched || f.current.bridge != nil {
				t.Fatal("tainted owner created canonical or executable callback work")
			}
		})
	}
}

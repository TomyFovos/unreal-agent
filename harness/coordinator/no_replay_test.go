package coordinator

import (
	"context"
	"encoding/json/v2"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

type noReplayAdapter struct{ llm.Adapter }

func (noReplayAdapter) NoAutomaticReplay() bool { return true }

func noReplayHistory(t *testing.T) []sessionstore.Item {
	t.Helper()
	return []sessionstore.Item{storedItem(1, sessionstore.ItemInput, externalEvent(t, 1, "initial-request", "Original explicit request")), storedItem(2, sessionstore.ItemTurn, session.Turn{ID: "unanswered-native", Type: session.TurnRegular, InputWatermark: 1, NoAutomaticReplay: true})}
}

func TestNativeReplayMarkerRestoreClassifiesWithoutInventingDelivery(t *testing.T) {
	for _, spec := range []struct {
		name    string
		change  func([]sessionstore.Item) []sessionstore.Item
		held    bool
		invalid bool
	}{
		{name: "crash_after_durable_marker", held: true},
		{name: "durable_success_clears_marker", change: func(items []sessionstore.Item) []sessionstore.Item {
			return append(items, storedItem(3, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "unanswered-native", Response: textResponse("completed")}))
		}},
		{name: "legacy_turn_unchanged", change: func(items []sessionstore.Item) []sessionstore.Item {
			turn := items[1].Data.(session.Turn)
			turn.NoAutomaticReplay = false
			turn.InputWatermark = 0
			items[1].Data = turn
			return items
		}},
		{name: "new_explicit_user_input", change: func(items []sessionstore.Item) []sessionstore.Item {
			return append(items, storedItem(3, sessionstore.ItemInput, externalEvent(t, 2, "new-request", "New explicit request")))
		}},
		{name: "queued_peer_does_not_authorize_replay", held: true, change: func(items []sessionstore.Item) []sessionstore.Item {
			raw, _ := json.Marshal(inbox.PeerMessage{Version: 1, Sender: "parent", Target: "session-1", Handle: "handle", Kind: "message", Text: "peer update"})
			return append(items, storedItem(3, sessionstore.ItemInput, inbox.Input{ID: "peer", Kind: inbox.InputPeer, Payload: raw}))
		}},
		{name: "invalid_watermark", invalid: true, change: func(items []sessionstore.Item) []sessionstore.Item {
			turn := items[1].Data.(session.Turn)
			turn.InputWatermark = 2
			items[1].Data = turn
			return items
		}},
	} {
		t.Run(spec.name, func(t *testing.T) {
			store := emptyFakeStore()
			store.items = noReplayHistory(t)
			if spec.change != nil {
				store.items = spec.change(store.items)
			}
			current := newTestCoordinator(store, newTestInbox(t), newFakeOperationManager(), contextbuilder.NewBuilder(), tool.NewRegistry(tool.StaticTranslators{}))
			err := current.restore(t.Context())
			if spec.invalid {
				if err == nil {
					t.Fatal("invalid marker accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if current.modelReplayHeld() != spec.held {
				t.Fatal("incorrect replay hold")
			}
			if spec.held && (current.pendingInputs() != 0 || current.state.deliveredInputs != 0 || len(store.appendedResponses) != 0) {
				t.Fatal("unknown result became delivered/successful")
			}
		})
	}
}

func TestNativeMarkerPersistenceFailureSendsZeroAndUnknownCommitStaysHeld(t *testing.T) {
	store := emptyFakeStore()
	store.items = noReplayHistory(t)[:1]
	store.appendTurnErr = errors.New("synthetic turn sync failure")
	adapter := &fakeAdapter{}
	current := newTestCoordinatorWithAdapter(store, newTestInbox(t), newFakeOperationManager(), contextbuilder.NewBuilder(), tool.NewRegistry(tool.StaticTranslators{}), adapter)
	current.dependencies.LLM = noReplayAdapter{adapter}
	if err := current.restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := current.requestModelResponse(t.Context(), make(chan modelResponseResult, 1)); err == nil {
		t.Fatal("turn persistence error ignored")
	}
	if len(adapter.requestSnapshot()) != 0 || len(store.appendedTurns) != 1 || !store.appendedTurns[0].NoAutomaticReplay || store.appendedTurns[0].InputWatermark != 1 {
		t.Fatal("model called before non-replay marker persisted")
	}
	// A Sync error can follow a committed append. Reconstruct exactly that
	// possible durable state; neither crash timing nor missing result permits a
	// guessed repeat of the request.
	restored := emptyFakeStore()
	restored.items = []sessionstore.Item{store.items[0], storedItem(2, sessionstore.ItemTurn, store.appendedTurns[0])}
	resumed := newTestCoordinatorWithAdapter(restored, newTestInbox(t), newFakeOperationManager(), contextbuilder.NewBuilder(), tool.NewRegistry(tool.StaticTranslators{}), adapter)
	if err := resumed.restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := resumed.requestModelResponse(t.Context(), make(chan modelResponseResult, 1)); err != nil {
		t.Fatal(err)
	}
	if len(adapter.requestSnapshot()) != 0 || !resumed.modelReplayHeld() {
		t.Fatal("unknown append outcome replayed")
	}
}

func TestNativeReplayHoldWaitsForExplicitInputAndThenMakesNewTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 0)
		run.store.items = noReplayHistory(t)
		run.current.dependencies.LLM = noReplayAdapter{run.current.dependencies.LLM}
		notices := 0
		run.current.dependencies.ModelReplayHeld = func(turn session.TurnID) {
			if turn != "unanswered-native" {
				t.Fatal("wrong hold correlation")
			}
			notices++
		}
		run.start(t)
		run.assertRunning(t)
		if len(run.calls) != 0 || notices != 1 {
			t.Fatal("restore replayed or failed to notify waiting state")
		}
		run.input(t, externalEvent(t, 2, "new-explicit-request", "A new explicit instruction after the unknown outcome."))
		if len(run.calls) != 1 {
			t.Fatal("new user instruction did not resume normal request flow")
		}
		if len(run.store.appendedTurns) != 1 || !run.store.appendedTurns[0].NoAutomaticReplay || run.store.appendedTurns[0].ID == "unanswered-native" || run.store.appendedTurns[0].InputWatermark != 2 {
			t.Fatal("new request reused prior canonical identity")
		}
		run.respond(t, 0, textResponse("new explicit request completed"))
		if run.current.modelReplayHeld() {
			t.Fatal("durable successful response left a replay hold")
		}
		run.input(t, stopInput(t, "stop", inbox.StopWhenIdle))
		run.assertStopped(t)
	})
}

func TestLegacyRequestDoesNotCreateNativeReplayMarker(t *testing.T) {
	store := emptyFakeStore()
	store.items = noReplayHistory(t)[:1]
	adapter := &fakeAdapter{respond: func(context.Context, llm.Request) (llm.Response, error) { return textResponse("legacy"), nil }}
	current := newTestCoordinatorWithAdapter(store, newTestInbox(t), newFakeOperationManager(), contextbuilder.NewBuilder(), tool.NewRegistry(tool.StaticTranslators{}), adapter)
	if err := current.restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan modelResponseResult, 1)
	if err := current.requestModelResponse(ctx, result); err != nil {
		t.Fatal(err)
	}
	receiveTestValue(t, result)
	if len(store.appendedTurns) != 1 || store.appendedTurns[0].NoAutomaticReplay || store.appendedTurns[0].InputWatermark != 0 {
		t.Fatal("legacy request semantics changed")
	}
}

type noReplayFailureStore struct {
	*fakeStore
	failBefore, failAfter bool
	records               int
}

func (s *noReplayFailureStore) AppendHostRecord(_ context.Context, _ session.ID, r sessionstore.HostRecord) error {
	if s.failBefore {
		return errors.New("synthetic failure before append")
	}
	if err := r.Validate(); err != nil {
		return err
	}
	s.records++
	s.items = append(s.items, storedItem(sessionstore.Sequence(len(s.items)+1), sessionstore.ItemHostRecord, r))
	if s.failAfter {
		return errors.New("synthetic failure after append")
	}
	return nil
}
func TestNativeTransportUnknownCannotCreateKnownFailureReceipt(t *testing.T) {
	for _, cause := range []error{&credential.Error{Code: "normal_send_result_unknown"}, context.DeadlineExceeded} {
		s := &noReplayFailureStore{fakeStore: emptyFakeStore()}
		s.items = noReplayHistory(t)
		current := newTestCoordinator(s.fakeStore, newTestInbox(t), newFakeOperationManager(), contextbuilder.NewBuilder(), tool.NewRegistry(tool.StaticTranslators{}))
		current.dependencies.Sessions = s
		if err := current.restore(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := current.processModelResponse(t.Context(), modelResponseResult{turnID: "unanswered-native", err: cause}); err == nil {
			t.Fatal("unknown result became successful")
		}
		if !current.modelReplayHeld() || s.records != 0 || len(s.appendedResponses) != 0 {
			t.Fatal("unknown transport discharged canonical marker")
		}
	}
}
func TestNativeKnownFailureForeignTurnCannotDischargeMarker(t *testing.T) {
	s := emptyFakeStore()
	s.items = noReplayHistory(t)
	s.items = append(s.items, storedItem(3, sessionstore.ItemHostRecord, sessionstore.HostRecord{Version: 1, Kind: sessionstore.HostModelRequestFailure, ModelRequestFailure: &sessionstore.ModelRequestFailure{TurnID: "foreign-turn", HTTPStatus: 503}}))
	current := newTestCoordinator(s, newTestInbox(t), newFakeOperationManager(), contextbuilder.NewBuilder(), tool.NewRegistry(tool.StaticTranslators{}))
	if err := current.restore(t.Context()); err == nil {
		t.Fatal("foreign failure discharged pending request")
	}
}
func TestNativeDurableContinuityEpochAllowsOnlyItsFailedRequest(t *testing.T) {
	f, s := newConversationFixture(t)
	marker := session.Turn{ID: "marked-continuity", PreviousTurnID: f.current.state.currentTurnID, Type: session.TurnRegular, NoAutomaticReplay: true, InputWatermark: f.current.state.availableInputs}
	item, err := f.current.addItemToLocalState(sessionstore.Item{Kind: sessionstore.ItemTurn, Data: marker})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.current.storeItemInSessionStore(t.Context(), item); err != nil {
		t.Fatal(err)
	}
	if err = f.current.processModelResponse(t.Context(), modelResponseResult{turnID: marker.ID, err: continuityFailure{}}); err != nil {
		t.Fatal(err)
	}
	if f.current.modelReplayHeld() || s.epochs != 1 || len(s.appendedResponses) != 0 || len(f.manager.adds) != 0 {
		t.Fatal("known continuity failed to preserve bounded recovery semantics")
	}
	restored := newTestCoordinator(s.fakeStore, newTestInbox(t), newFakeOperationManager(), contextbuilder.NewBuilder(), f.registry)
	restored.dependencies.Sessions = s
	if err = restored.restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	if restored.modelReplayHeld() {
		t.Fatal("committed continuity decision was lost at restart")
	}
}

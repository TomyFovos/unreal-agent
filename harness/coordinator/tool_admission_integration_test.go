package coordinator

import (
	"errors"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
)

// Uses the actual append-only localfile Store and a newly constructed
// Coordinator after reopening it. Operation workers remain synthetic value
// workers: this is durable admission/receipt integration, not actual Bash I/O.
func TestToolAdmissionLocalfileRestartCompactionAndFork(t *testing.T) {
	for _, mode := range []string{"restart", "compaction-restart", "fork"} {
		t.Run(mode, func(t *testing.T) {
			f := newToolAdmissionFixture(t)
			directory := t.TempDir()
			store, err := localfile.New(directory)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Create(t.Context(), "session-1"); err != nil {
				t.Fatal(err)
			}
			f.current.dependencies.Sessions = store
			// The fixture's initial turn was previously committed to its fake;
			// explicitly persist that exact turn in this independent real Store.
			if err := store.AppendTurn(t.Context(), "session-1", session.Turn{ID: "first", Type: session.TurnRegular}); err != nil {
				t.Fatal(err)
			}
			call := admissionCall("persisted-id", `{"path":"synthetic.txt"}`)
			response := admissionResponse("first", call)
			if _, err := f.current.handleModelResponse(t.Context(), response); err != nil {
				t.Fatal(err)
			}
			if err := f.current.dispatchOperationsToManager(t.Context()); err != nil {
				t.Fatal(err)
			}
			if len(f.manager.adds) != 1 {
				t.Fatal("first admission did not dispatch exactly once")
			}
			completed := f.manager.adds[0]
			completed.Status = operation.StatusCompleted
			if err := f.current.handleOperationUpdate(t.Context(), completed); err != nil {
				t.Fatal(err)
			}
			if _, err := f.current.reconcileToolCalls(t.Context()); err != nil {
				t.Fatal(err)
			}
			page, err := store.Items(t.Context(), "session-1", sessionstore.BeforeFirst, 100)
			if err != nil {
				t.Fatal(err)
			}
			var found bool
			for _, item := range page.Items {
				if status, ok := item.Data.(sessionstore.ToolCallStatus); ok && status.CallID == call.CallID && len(status.Operations) == 1 && status.Operations[0].ID == completed.ID && status.Operations[0].Status == operation.StatusCompleted {
					found = true
				}
			}
			if !found {
				t.Fatal("native Store lacks the original Tool Call/Operation terminal receipt")
			}
			sessionID := session.ID("session-1")
			switch mode {
			case "compaction-restart":
				item := sessionstore.Item{Kind: sessionstore.ItemTurn, Data: session.Turn{ID: "compact", Type: session.TurnCompaction, PreviousTurnID: "first"}}
				if _, err := f.current.addItemToLocalState(item); err != nil {
					t.Fatal(err)
				}
				if err := f.current.storeItemInSessionStore(t.Context(), item); err != nil {
					t.Fatal(err)
				}
				if _, err := f.current.handleModelResponse(t.Context(), sessionstore.ModelResponse{TurnID: "compact", Response: textResponse("synthetic compact summary without the Tool Call ID")}); err != nil {
					t.Fatal(err)
				}
			case "fork":
				if _, err := store.Fork(t.Context(), "child", "session-1", "first"); err != nil {
					t.Fatal(err)
				}
				sessionID = "child"
			}
			reopened, err := localfile.New(directory)
			if err != nil {
				t.Fatal(err)
			}
			restored, err := reopened.Resume(t.Context(), sessionID)
			if err != nil {
				t.Fatal(err)
			}
			manager := newFakeOperationManager()
			current := New(Dependencies{SessionID: sessionID, Inbox: newTestInbox(t), Sessions: reopened, Restored: restored, ContextBuilder: contextbuilder.NewBuilder(), Operations: manager, Tools: f.registry}).(*coordinator)
			if err := current.restore(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, err := current.scheduleToolCalls(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := current.dispatchOperationsToManager(t.Context()); err != nil {
				t.Fatal(err)
			}
			if len(manager.adds) != 0 {
				t.Fatal("restoring a durable terminal receipt redispatched its Operation")
			}
			before, err := reopened.Items(t.Context(), sessionID, sessionstore.BeforeFirst, 100)
			if err != nil {
				t.Fatal(err)
			}
			_, err = current.handleModelResponse(t.Context(), admissionResponse("new-generation", call))
			if !errors.Is(err, llm.ErrToolRecoveryRequired) {
				t.Fatalf("persisted identity re-proposal = %v, want RecoveryRequired", err)
			}
			after, err := reopened.Items(t.Context(), sessionID, sessionstore.BeforeFirst, 100)
			if err != nil {
				t.Fatal(err)
			}
			if len(after.Items) != len(before.Items) || len(manager.adds) != 0 {
				t.Fatal("duplicate after reopen generated canonical or executable work")
			}
			t.Logf("mode=%s session_id=%s call_id=%s original_operation_id=%s new_dispatches=%d canonical_items=%d", mode, sessionID, call.CallID, completed.ID, len(manager.adds), len(after.Items))
		})
	}
}

package host

import (
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"testing"
)

func TestOperationPostCommitNotificationAndResync(t *testing.T) {
	h := newTestHost(t, t.TempDir())
	s, err := h.Create(t.Context(), Options{ID: "ops"})
	if err != nil {
		t.Fatal(err)
	}
	sub, err := s.Subscribe(0, 4096, 16)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Cancel()
	store := &serializedStore{s}
	if err = store.AppendTurn(t.Context(), s.ID, session.Turn{ID: "turn", Type: session.TurnRegular}); err != nil {
		t.Fatal(err)
	}
	op := operation.Operation{ID: "op", Type: "test", Version: 1, Status: operation.StatusReady}
	if err = store.AppendToolCallStatus(t.Context(), s.ID, sessionstore.ToolCallStatus{TurnID: "turn", CallID: "call", Status: tool.CallStatus{WaitingFor: []operation.ID{op.ID}}, Operations: []operation.Operation{op}}); err != nil {
		t.Fatal(err)
	}
	op.Status = operation.StatusAwaiting
	if err = store.SaveOperation(t.Context(), s.ID, op); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case event := <-sub.Events:
			if event.Kind != "operation" {
				continue
			}
			persisted, err := s.store.Operations(t.Context(), s.ID)
			if err != nil || len(persisted) != 1 || persisted[0].Status != operation.StatusAwaiting {
				t.Fatal("notification preceded commit", err)
			}
			view, err := s.Inspect(0, 4096)
			if err != nil || len(view.Operations) != 1 || view.Operations[0].Status != operation.StatusAwaiting {
				t.Fatal("resync operation missing", err)
			}
			if event.Operation == nil || event.Operation.ID != op.ID {
				t.Fatal("missing notification payload")
			}
			return
		case <-timeout(t).Done():
			t.Fatal("no operation notification")
		}
	}
}

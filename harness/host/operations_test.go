package host

import (
	"context"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"testing"
)

func TestOperationPostCommitNotificationAndResync(t *testing.T) {
	started := make(chan struct{})
	h, err := New(t.Context(), Config{Directory: t.TempDir(), Build: factory(modelFunc(func(ctx context.Context, _ llm.Request, _ llm.RequestOptions) (llm.Response, error) {
		close(started)
		<-ctx.Done()
		return llm.Response{}, ctx.Err()
	}))})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	s, err := h.Create(t.Context(), Options{ID: "ops"})
	if err != nil {
		t.Fatal(err)
	}
	sub, err := s.Subscribe(0, 4096, 16)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Cancel()
	if _, err = s.Submit(timeout(t), s.Generation, input("start", "hold model")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-timeout(t).Done():
		t.Fatal("coordinator did not start")
	}
	store := &serializedStore{s}
	snapshot, err := s.Inspect(0, 4096)
	if err != nil {
		t.Fatal(err)
	}
	var turnID session.TurnID
	for _, item := range snapshot.History.Items {
		if item.Kind == sessionstore.ItemTurn {
			turnID = item.Data.(session.Turn).ID
		}
	}
	if turnID == "" {
		t.Fatal("missing live turn")
	}

	op := operation.Operation{ID: "op", Type: "test", Version: 1, Status: operation.StatusReady}
	if err = store.AppendToolCallStatus(t.Context(), s.ID, sessionstore.ToolCallStatus{TurnID: turnID, CallID: "call", Status: tool.CallStatus{WaitingFor: []operation.ID{op.ID}}, Operations: []operation.Operation{op}}); err != nil {
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

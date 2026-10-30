package subagent

import (
	"context"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/operation"
)

func TestSendParentMessageKind(t *testing.T) {
	for _, progress := range []bool{false, true} {
		kind := "message"
		if progress {
			kind = "progress"
		}
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			inputs := make(chan inbox.Input, 1)
			manager, err := NewManager(ctx, Config{
				Owner: &host.Session{ID: "child"},
				Child: &ChildConfig{ChildID: "child", ParentID: "parent", OperationID: "child-op"},
				SendParent: func(_ context.Context, input inbox.Input) (host.Receipt, error) {
					inputs <- input
					return host.Receipt{ID: input.ID, Sequence: 1}, nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer manager.Close()
			spec, err := NewSpec(Plan{Version: 1, Action: "parent", ParentID: "child", Text: "report", Progress: progress})
			if err != nil {
				t.Fatal(err)
			}
			op := operation.Operation{ID: "send-op", Type: spec.Type, Version: spec.Version, State: spec.State, MaxOutputLength: spec.MaxOutputLength, Status: operation.StatusReady}
			if err := manager.AddRemoteJob(op); err != nil {
				t.Fatal(err)
			}
			select {
			case update := <-manager.RemoteJobUpdates():
				if update.Status != operation.StatusCompleted {
					t.Fatal("send failed", update)
				}
			case <-ctx.Done():
				t.Fatal("send did not complete", ctx.Err())
			}
			select {
			case input := <-inputs:
				peer, err := input.DecodePeerMessage()
				if err != nil || input.ID != "send:send-op" || peer.Kind != kind || peer.Sender != "child" || peer.Target != "parent" || peer.Handle != "child-op" || peer.Text != "report" {
					t.Fatal(input, peer, err)
				}
			default:
				t.Fatal("send completed without delivering its peer message")
			}
		})
	}
}

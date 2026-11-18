package host

import (
	"context"
	"encoding/json/v2"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
)

// Both shutdown (while holding Session.mu) and offline reconciliation must use
// the stopped parent's records, not the active runtime owner's input IDs.
func TestStoppedChildCleanupUsesCanonicalParentDelivery(t *testing.T) {
	for _, offline := range []bool{false, true} {
		for _, delivered := range []bool{false, true} {
			name := "shutdown"
			if offline {
				name = "offline"
			}
			if delivered {
				name += "/delivered"
			} else {
				name += "/undelivered"
			}
			t.Run(name, func(t *testing.T) {
				store, err := localfile.New(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				if _, err = store.Create(t.Context(), "stopped-parent"); err != nil {
					t.Fatal(err)
				}
				spec, err := operation.NewRemoteJobSpec(operation.RemoteJobPlan{Type: "subagent", Version: 1, Data: []byte(`{"Action":"start"}`)})
				if err != nil {
					t.Fatal(err)
				}
				raw, _ := json.Marshal(spec)
				payload, _ := json.Marshal(inbox.ControlMessage{Mode: inbox.DispatchOperation, Parameters: inbox.OperationIntent{OperationID: "start", ToolName: "SubagentStart", Spec: raw}})
				if err = store.AppendInput(t.Context(), "stopped-parent", inbox.Input{ID: "intent", Kind: inbox.InputControl, Payload: payload}); err != nil {
					t.Fatal(err)
				}
				if delivered {
					payload, _ = json.Marshal(inbox.PeerMessage{Version: 1, Sender: "first-child", Target: "stopped-parent", Handle: "first", Kind: "ended", Text: "Child first ended: completed\n(Full report: subagent show first)\n\nChild start ended: canceled\n(Full report: subagent show start)"})
					if err = store.AppendInput(t.Context(), "stopped-parent", inbox.Input{ID: "legacy-batch", Kind: inbox.InputPeer, Payload: payload}); err != nil {
						t.Fatal(err)
					}
				}
				retained, notified := 0, 0
				retain := func(context.Context, operation.Operation) (bool, error) {
					retained++
					return false, nil
				}
				notice := func(operation.Operation) (*inbox.Input, error) {
					notified++
					payload, _ := json.Marshal(inbox.PeerMessage{Version: 1, Sender: "child", Target: "stopped-parent", Handle: "start", Kind: "ended", Text: "executor lost"})
					return &inbox.Input{ID: "restarted:start", Kind: inbox.InputPeer, Payload: payload}, nil
				}
				if offline {
					err = reconcileStoppedChildren(t.Context(), store, "stopped-parent", notice, retain)
				} else {
					s := &Session{ID: "stopped-parent", store: store, operations: map[operation.ID]operation.Operation{}, submissions: map[inbox.ID]*submission{}}
					s.runtime.RetainOperation, s.runtime.InterruptedInput = retain, notice
					_, items, ops, readErr := store.ReadSnapshot(t.Context(), s.ID)
					if readErr != nil {
						t.Fatal(readErr)
					}
					for _, item := range items {
						s.rememberOwnedItem(item, false)
					}
					for _, op := range ops {
						s.setOperation(op)
					}
					err = s.cancelStoppedChildren(t.Context(), true)
				}
				if err != nil {
					t.Fatal(err)
				}
				want := 1
				if delivered {
					want = 0
				}
				if retained != 1 || notified != want {
					t.Fatal("executor reconciliation/notifications", retained, notified, "want", 1, want)
				}
				ops, err := store.Operations(t.Context(), "stopped-parent")
				if err != nil || len(ops) != 1 || ops[0].Status != operation.StatusCanceled {
					t.Fatal("abandoned child not canceled", ops, err)
				}
			})
		}
	}
}

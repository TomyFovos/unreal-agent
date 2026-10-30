package subagent_test

import (
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	sub "github.com/unreallabsai/unreal-agent/harness/subagent"
)

func TestSendToEndedChildAcknowledgesStatusAndReport(t *testing.T) {
	for _, task := range []string{"finish", "failed", "crash", "wait"} {
		t.Run(task, func(t *testing.T) {
			h, parent := newParent(t, task)
			defer h.Close()
			active := awaitChild(t, parent, task != "wait")
			plan, err := sub.DecodePlan(active)
			if err != nil {
				t.Fatal(err)
			}
			request := sub.ControlRequest{ParentID: parent.ID, ParentGeneration: parent.Generation, OperationID: active.ID, ChildID: plan.ChildID, InputID: "send-ended", Text: "continue"}
			if task == "wait" {
				waitReady(t, parent)
				request.InputID = inbox.ID("cancel-ended")
				if _, err = sub.CancelChild(t.Context(), parent, request); err != nil {
					t.Fatal(err)
				}
				active = awaitChild(t, parent, true)
				request.InputID = "send-ended"
			}
			if _, err = sub.SteerChild(t.Context(), parent, request); err != nil {
				t.Fatal(err)
			}
			awaitBoundary(t, func() bool {
				v, err := parent.Inspect(0, 256)
				if err != nil {
					t.Fatal(err)
				}
				for _, op := range v.Operations {
					if op.ToolName != "SubagentSend" || op.Status == operation.StatusReady || op.Status == operation.StatusAwaiting {
						continue
					}
					state, _ := operation.DecodeRemoteJobState(op)
					if op.Status != operation.StatusCompleted || !strings.Contains(state.TerminalResult, "status: "+string(active.Status)) || !strings.Contains(state.TerminalResult, "subagent show "+string(active.ID)) {
						t.Fatal(op.Status, state.TerminalResult, state.TerminalError)
					}
					return true
				}
				return false
			})
		})
	}
}

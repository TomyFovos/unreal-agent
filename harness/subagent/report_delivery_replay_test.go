package subagent_test

import (
	"context"
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	sub "github.com/unreallabsai/unreal-agent/harness/subagent"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"github.com/unreallabsai/unreal-agent/harness/tool/observed"
)

// Reopen the same committed parent/child records with fresh Host and Manager
// state. Fixtures model the old batches and receipts without calling a model
// provider or relying on the interval between completion and restart.
func TestChildReportDeliveryReplayAcrossRestarts(t *testing.T) {
	for _, test := range []struct {
		name, delivery string
		status         operation.Status
		finish         bool
		wantNotice     string
	}{
		{"finished_peer_delivered", "peer", operation.StatusCompleted, true, ""},
		{"finished_legacy_batch_delivered", "batch", operation.StatusCompleted, true, ""},
		{"failed_legacy_batch_delivered", "batch", operation.StatusFailed, true, ""},
		{"canceled_peer_delivered", "peer", operation.StatusCanceled, false, ""},
		{"canceled_show_delivered_without_finish", "show_canceled", operation.StatusCanceled, false, ""},
		{"finished_terminal_start_receipt", "receipt", operation.StatusCompleted, true, ""},
		{"canceled_terminal_start_receipt", "receipt", operation.StatusCanceled, false, ""},
		{"finished_show_tail_delivered", "show_tail", operation.StatusCompleted, true, ""},
		{"finished_undelivered", "", operation.StatusCompleted, true, "ended:start"},
		{"failed_undelivered", "", operation.StatusFailed, true, "ended:start"},
		{"canceled_undelivered", "", operation.StatusCanceled, false, "restarted:start"},
		{"orphan_without_finish", "", operation.StatusAwaiting, false, "restarted:start"},
		{"orphan_end_already_delivered", "peer", operation.StatusAwaiting, false, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			childHost, child, op := restartFixture(t, dir)
			childHost.Close()
			initial := op
			if test.delivery == "receipt" {
				// Tool-generated starts initialize through their first receipt,
				// rather than the CLI's durable dispatch-operation input.
				prefix := logBoundary(t, readLog(t, dir, "parent"), false, inputBoundary("start-intent"))
				restoreLog(t, dir, "parent", prefix)
			}
			store, err := localfile.New(dir)
			if err != nil {
				t.Fatal(err)
			}
			var finish *sessionstore.FinishRecord
			if test.finish {
				status := "completed"
				if test.status == operation.StatusFailed {
					status = "failed"
				}
				finish = &sessionstore.FinishRecord{Version: 1, OperationID: "finish", Result: sessionstore.FinishResult{Status: status, Summary: "canonical child report", ChangedFiles: []string{"fixture.go"}, Tests: []string{"focused: PASS"}}}
				if err = store.AppendHostRecord(t.Context(), child.ID, sessionstore.HostRecord{Version: 1, Kind: "finish", Finish: finish}); err != nil {
					t.Fatal(err)
				}
			}
			state, _ := operation.DecodeRemoteJobState(op)
			if finish != nil {
				state.Handle, _ = json.Marshal(sub.Handle{Version: 1, ParentID: "parent", ChildID: child.ID, OperationID: op.ID, Finish: finish})
				state.TerminalResult = finish.Result.Summary
			}
			step, err := operation.UpdateRemoteJob(op, state, test.status)
			if err != nil {
				t.Fatal(err)
			}
			op = *step.Operation
			if test.delivery != "receipt" {
				if err = store.SaveOperation(t.Context(), "parent", op); err != nil {
					t.Fatal(err)
				}
			}
			switch test.delivery {
			case "peer", "batch":
				peer := inbox.PeerMessage{Version: 1, Sender: string(child.ID), Target: "parent", Handle: "start", Kind: "ended", Text: "Child start ended: " + string(test.status) + "\n(Full report: subagent show start)"}
				if test.delivery == "batch" {
					peer.Sender, peer.Handle = "first-child", "first"
					peer.Text = "Child first ended: completed\nSummary: another child\n(Full report: subagent show first)\n\n" + peer.Text
				}
				payload, _ := json.Marshal(peer)
				// Different input identity, as with a legacy batched notice.
				if err = store.AppendInput(t.Context(), "parent", inbox.Input{ID: "historical-end", Kind: inbox.InputPeer, Payload: payload}); err != nil {
					t.Fatal(err)
				}
			case "receipt":
				turn := session.Turn{ID: "reported", Type: session.TurnRegular}
				if err = store.AppendTurn(t.Context(), "parent", turn); err != nil {
					t.Fatal(err)
				}
				call := llm.ToolCall{CallID: "start-call", Name: "SubagentStart", Arguments: `{"template":"default","task":"continue existing work"}`}
				if err = store.AppendModelResponse(t.Context(), "parent", sessionstore.ModelResponse{TurnID: turn.ID, Response: llm.Response{Output: []llm.Item{{Type: llm.ItemToolCall, Data: call}}}}); err != nil {
					t.Fatal(err)
				}
				if err = store.AppendToolCallStatus(t.Context(), "parent", sessionstore.ToolCallStatus{TurnID: turn.ID, CallID: call.CallID, Status: tool.CallStatus{WaitingFor: []operation.ID{op.ID}}, Operations: []operation.Operation{initial}}); err != nil {
					t.Fatal(err)
				}
				if err = store.SaveOperation(t.Context(), "parent", op); err != nil {
					t.Fatal(err)
				}
				if err = store.AppendToolCallStatus(t.Context(), "parent", sessionstore.ToolCallStatus{TurnID: turn.ID, CallID: call.CallID, Status: tool.CallStatus{WaitingFor: []operation.ID{op.ID}}, Operations: []operation.Operation{op}}); err != nil {
					t.Fatal(err)
				}
			case "show_tail", "show_canceled":
				report := "=== start\nlast summary lines\nchanged files:\n  - fixture.go\ntests:\n  - focused: PASS\nblockers: none\n"
				if test.delivery == "show_canceled" {
					report = "== other-child  worker  awaiting\n== start  worker  canceled\n"
				}
				value, _ := json.Marshal(observed.Result{Tool: "Bash", Content: report})
				spec, err := operation.NewValueSpec(value)
				if err != nil {
					t.Fatal(err)
				}
				raw, _ := json.Marshal(spec)
				payload, _ := json.Marshal(inbox.ControlMessage{Mode: inbox.DispatchOperation, Parameters: inbox.OperationIntent{OperationID: "read-report", ToolName: "claude_code", Spec: raw}})
				if err = store.AppendInput(t.Context(), "parent", inbox.Input{ID: "read-intent", Kind: inbox.InputControl, Payload: payload}); err != nil {
					t.Fatal(err)
				}
				if err = store.SaveOperation(t.Context(), "parent", operation.Operation{ID: "read-report", Type: spec.Type, Version: spec.Version, State: spec.State, Status: operation.StatusCompleted}); err != nil {
					t.Fatal(err)
				}
			}
			before := readLog(t, dir, child.ID)
			// Repeated restarts must preserve Finish/end records and never
			// append another notice, regardless of the old delivery mechanism.
			for restart := range 3 {
				h, parent, _ := boundaryParent(t, dir, host.Resume)
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				if _, err = parent.Stop(ctx, parent.Generation, inbox.StopWhenIdle, "drain replay"); err != nil {
					cancel()
					t.Fatal(err)
				}
				if err = parent.Wait(ctx); err != nil {
					cancel()
					t.Fatal(err)
				}
				cancel()
				view, err := parent.Inspect(0, 256)
				if err != nil || view.History.More {
					t.Fatal("incomplete fixture history", err)
				}
				var notices []inbox.Input
				for _, item := range view.History.Items {
					if input, ok := item.Data.(inbox.Input); ok && (input.ID == "ended:start" || input.ID == "restarted:start") {
						notices = append(notices, input)
					}
				}
				if test.wantNotice == "" {
					if len(notices) != 0 {
						t.Fatalf("restart %d repeated an already delivered end: %+v", restart, notices)
					}
				} else {
					if len(notices) != 1 || string(notices[0].ID) != test.wantNotice {
						t.Fatalf("restart %d lost or duplicated an undelivered end: %+v", restart, notices)
					}
					peer, err := notices[0].DecodePeerMessage()
					if err != nil || peer.Handle != "start" || peer.Kind != "ended" || test.wantNotice == "restarted:start" && !strings.Contains(peer.Text, "ended_by=host_restart") {
						t.Fatal(peer, err)
					}
				}
				assertRestartEndOnly(t, dir, child.ID, before)
				h.Close()
			}
		})
	}
}

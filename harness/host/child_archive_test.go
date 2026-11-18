package host

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	"github.com/unreallabsai/unreal-agent/harness/subagent/identity"
)

func childArchiveFixture(t *testing.T, status operation.Status, delivered, question, approval bool) (*localfile.Store, session.ID) {
	t.Helper()
	store, err := localfile.New(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	id := identity.ChildID("parent", "start")
	for _, sid := range []session.ID{"parent", id} {
		if _, err = store.CreateWithHostRecords(t.Context(), sid, sessionstore.HostRecord{Version: 1, Kind: "configuration", Configuration: []byte(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	plan, _ := json.Marshal(archivePlan{Version: 1, Action: "start", ParentID: "parent", ChildID: id})
	spec, err := operation.NewRemoteJobSpec(operation.RemoteJobPlan{Type: "subagent", Version: 1, Data: plan})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(spec)
	payload, _ := json.Marshal(inbox.ControlMessage{Mode: inbox.DispatchOperation, Parameters: inbox.OperationIntent{OperationID: "start", ToolName: "SubagentStart", Spec: raw}})
	in := inbox.Input{ID: "start-intent", Kind: inbox.InputControl, Payload: payload}
	if err = store.AppendInput(t.Context(), "parent", in); err != nil {
		t.Fatal(err)
	}
	op, err := sessionstore.DecodeOperationIntent(in)
	if err != nil {
		t.Fatal(err)
	}
	state, _ := operation.DecodeRemoteJobState(op)
	if approval {
		state.Handle = []byte(`{"Approvals":[{"ID":"unanswered-approval"}]}`)
	}
	step, err := operation.UpdateRemoteJob(op, state, status)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.SaveOperation(t.Context(), "parent", *step.Operation); err != nil {
		t.Fatal(err)
	}
	if question {
		appendArchivePeer(t, store, id, "question", "message")
	}
	if delivered {
		appendArchivePeer(t, store, id, "end", "ended")
	}
	return store, id
}

func appendArchivePeer(t *testing.T, store *localfile.Store, child session.ID, inputID inbox.ID, kind string) {
	t.Helper()
	payload, _ := json.Marshal(inbox.PeerMessage{Version: 1, Sender: string(child), Target: "parent", Handle: "start", Kind: kind, Text: "canonical peer input"})
	if err := store.AppendInput(t.Context(), "parent", inbox.Input{ID: inputID, Kind: inbox.InputPeer, Payload: payload}); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveSettlementRequiresEveryCanonicalCondition(t *testing.T) {
	for _, tt := range []struct {
		name                          string
		status                        operation.Status
		delivered, question, approval bool
		live                          bool
		want                          bool
	}{
		{name: "settled", status: operation.StatusCompleted, delivered: true, want: true},
		{name: "failed_settled", status: operation.StatusFailed, delivered: true, want: true},
		{name: "canceled_settled", status: operation.StatusCanceled, delivered: true, want: true},
		{name: "pending", status: operation.StatusAwaiting, delivered: true},
		{name: "undelivered", status: operation.StatusCompleted},
		{name: "live_executor", status: operation.StatusCompleted, delivered: true, live: true},
		{name: "unanswered_question", status: operation.StatusCompleted, delivered: true, question: true},
		{name: "unanswered_approval", status: operation.StatusCompleted, delivered: true, approval: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store, child := childArchiveFixture(t, tt.status, tt.delivered, tt.question, tt.approval)
			if tt.live {
				lease, err := store.AcquireWriter(child)
				if err != nil {
					t.Fatal(err)
				}
				defer lease.Close()
			}
			if err := archiveStoppedChildren(t.Context(), store, "parent"); err != nil {
				t.Fatal(err)
			}
			_, err := store.ArchiveIndex(child)
			if tt.want && err != nil || !tt.want && !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("archived=%v want=%v: %v", err == nil, tt.want, err)
			}
		})
	}
}

func TestArchiveRuntimeDeliveryAndExecutorExit(t *testing.T) {
	for _, waitForLease := range []bool{false, true} {
		t.Run(map[bool]string{false: "report_delivered_during_runtime", true: "executor_exits_after_delivery"}[waitForLease], func(t *testing.T) {
			store, child := childArchiveFixture(t, operation.StatusCompleted, waitForLease, false, false)
			var lease *os.File
			if waitForLease {
				var err error
				lease, err = store.AcquireWriter(child)
				if err != nil {
					t.Fatal(err)
				}
				defer lease.Close()
			}
			path, err := store.ResolveSessionPath("parent")
			if err != nil {
				t.Fatal(err)
			}
			h := newTestHost(t, filepath.Dir(path))
			s, err := h.Resume(t.Context(), Options{ID: "parent"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.ArchiveIndex(child); !errors.Is(err, fs.ErrNotExist) {
				t.Fatal("unsettled child was archived at start", err)
			}
			if waitForLease {
				if err = lease.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				payload, _ := json.Marshal(inbox.PeerMessage{Version: 1, Sender: string(child), Target: "parent", Handle: "start", Kind: "ended", Text: "canonical report"})
				_, err = s.SubmitPeer(t.Context(), s.Generation, string(child), "start", inbox.Input{ID: "runtime-end", Kind: inbox.InputPeer, Payload: payload})
				if err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			for {
				if _, err := store.ArchiveIndex(child); err == nil {
					break
				} else if !errors.Is(err, fs.ErrNotExist) {
					t.Fatal(err)
				}
				select {
				case <-ctx.Done():
					t.Fatal("runtime never archived settled child", ctx.Err())
				case <-time.After(10 * time.Millisecond):
				}
			}
			h.Close()
			index, err := store.ArchiveIndex(child)
			if err != nil {
				t.Fatal(err)
			}
			before, err := s.Inspect(0, 100)
			if err != nil {
				t.Fatal(err)
			}
			for range 3 {
				h = newTestHost(t, filepath.Dir(path))
				s, err = h.Resume(t.Context(), Options{ID: "parent"})
				if err != nil {
					t.Fatal(err)
				}
				h.Close()
				after, err := s.Inspect(0, 100)
				if err != nil || !reflect.DeepEqual(before.Operations, after.Operations) {
					t.Fatal("restart changed parent's child projection", err)
				}
				got, err := store.ArchiveIndex(child)
				if err != nil || !reflect.DeepEqual(index, got) {
					t.Fatal("repeated start rewrote archive index", err)
				}
			}
		})
	}
}

func TestArchiveQuestionRequiresDeliveredParentAnswer(t *testing.T) {
	for _, tt := range []struct {
		name      string
		status    operation.Status
		delivered bool
	}{
		{"delivered", operation.StatusCompleted, true},
		{"completed_without_child_receipt", operation.StatusCompleted, false},
		{"failed_without_child_receipt", operation.StatusFailed, false},
		{"receipt_before_failed_parent_checkpoint", operation.StatusFailed, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store, child := childArchiveFixture(t, operation.StatusCompleted, true, true, false)
			plan, _ := json.Marshal(archivePlan{Version: 1, Action: "send", ParentID: "parent", Handle: "start"})
			spec, err := operation.NewRemoteJobSpec(operation.RemoteJobPlan{Type: "subagent", Version: 1, Data: plan})
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(spec)
			payload, _ := json.Marshal(inbox.ControlMessage{Mode: inbox.DispatchOperation, Parameters: inbox.OperationIntent{OperationID: "answer", ToolName: "SubagentSend", Spec: raw}})
			in := inbox.Input{ID: "answer-intent", Kind: inbox.InputControl, Payload: payload}
			if err = store.AppendInput(t.Context(), "parent", in); err != nil {
				t.Fatal(err)
			}
			op, err := sessionstore.DecodeOperationIntent(in)
			if err != nil {
				t.Fatal(err)
			}
			state, _ := operation.DecodeRemoteJobState(op)
			step, err := operation.UpdateRemoteJob(op, state, tt.status)
			if err != nil {
				t.Fatal(err)
			}
			if err = store.SaveOperation(t.Context(), "parent", *step.Operation); err != nil {
				t.Fatal(err)
			}
			if tt.delivered {
				payload, _ := json.Marshal(inbox.PeerMessage{Version: 1, Sender: "parent", Target: string(child), Handle: "start", Kind: "message", Text: "canonical answer"})
				if err := store.AppendInput(t.Context(), child, inbox.Input{ID: "send:answer", Kind: inbox.InputPeer, Payload: payload}); err != nil {
					t.Fatal(err)
				}
			}
			if err = archiveStoppedChildren(t.Context(), store, "parent"); err != nil {
				t.Fatal(err)
			}
			_, err = store.ArchiveIndex(child)
			if tt.delivered && err != nil || !tt.delivered && !errors.Is(err, fs.ErrNotExist) {
				t.Fatal("answer delivery was inferred from its parent's checkpoint", err)
			}
		})
	}
}

func TestArchiveAnsweredApprovalUsesCanonicalDecision(t *testing.T) {
	for _, status := range []operation.Status{operation.StatusCompleted, operation.StatusFailed} {
		t.Run(string(status), func(t *testing.T) {
			store, child := childArchiveFixture(t, operation.StatusCompleted, true, false, true)
			plan, _ := json.Marshal(archivePlan{Version: 1, Action: "approve", ParentID: "parent", Handle: "start", Request: "unanswered-approval"})
			spec, err := operation.NewRemoteJobSpec(operation.RemoteJobPlan{Type: "subagent", Version: 1, Data: plan})
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(spec)
			payload, _ := json.Marshal(inbox.ControlMessage{Mode: inbox.DispatchOperation, Parameters: inbox.OperationIntent{OperationID: "decision", ToolName: "SubagentApprove", Spec: raw}})
			in := inbox.Input{ID: "decision-intent", Kind: inbox.InputControl, Payload: payload}
			if err = store.AppendInput(t.Context(), "parent", in); err != nil {
				t.Fatal(err)
			}
			op, err := sessionstore.DecodeOperationIntent(in)
			if err != nil {
				t.Fatal(err)
			}
			state, _ := operation.DecodeRemoteJobState(op)
			step, err := operation.UpdateRemoteJob(op, state, status)
			if err != nil {
				t.Fatal(err)
			}
			if err = store.SaveOperation(t.Context(), "parent", *step.Operation); err != nil {
				t.Fatal(err)
			}
			if err = archiveStoppedChildren(t.Context(), store, "parent"); err != nil {
				t.Fatal(err)
			}
			_, err = store.ArchiveIndex(child)
			if status == operation.StatusCompleted && err != nil || status == operation.StatusFailed && !errors.Is(err, fs.ErrNotExist) {
				t.Fatal("approval decision outcome ignored", err)
			}
		})
	}
}

func TestStoppedParentArchivesChildAfterExecutorExitsDuringRuntime(t *testing.T) {
	store, child := childArchiveFixture(t, operation.StatusCompleted, true, false, false)
	lease, err := store.AcquireWriter(child)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	path, err := store.ResolveSessionPath("parent")
	if err != nil {
		t.Fatal(err)
	}
	h := newTestHost(t, filepath.Dir(path))
	if _, err := h.Create(t.Context(), Options{ID: "another-session"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ArchiveIndex(child); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("stopped parent's live executor was archived", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for {
		if entry, err := store.ArchiveIndex(child); err == nil && !entry.Pending {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("stopped parent's settled child was never archived", ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if _, err := h.Attach("parent"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("archive maintenance resumed the stopped parent", err)
	}
}

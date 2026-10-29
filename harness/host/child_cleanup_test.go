package host

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
)

func TestStartupReconcilesStoppedSessionsOnceWithoutResuming(t *testing.T) {
	dir := t.TempDir()
	store, err := localfile.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := operation.NewRemoteJobSpec(operation.RemoteJobPlan{Type: "subagent", Version: 1, Data: jsontext.Value(`{"Action":"start"}`)})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []session.ID{"stopped-lead", "live-lead"} {
		if _, err = store.Create(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		for _, status := range []operation.Status{operation.StatusReady, operation.StatusAwaiting, operation.StatusCanceling, operation.StatusCompleted, operation.StatusFailed, operation.StatusCanceled} {
			op := operation.Operation{ID: operation.ID(status), ToolName: "SubagentStart", Type: spec.Type, Version: spec.Version, State: spec.State, MaxOutputLength: spec.MaxOutputLength, Status: status}
			raw, err := json.Marshal(spec)
			if err != nil {
				t.Fatal(err)
			}
			payload, err := json.Marshal(inbox.ControlMessage{Mode: inbox.DispatchOperation, Parameters: inbox.OperationIntent{OperationID: string(op.ID), ToolName: op.ToolName, Spec: raw}})
			if err != nil {
				t.Fatal(err)
			}
			if err = store.AppendInput(t.Context(), id, inbox.Input{ID: inbox.ID(status), Kind: inbox.InputControl, Payload: payload}); err != nil {
				t.Fatal(err)
			}
			if err = store.SaveOperation(t.Context(), id, op); err != nil {
				t.Fatal(err)
			}
		}
	}
	live, err := store.AcquireWriter("live-lead")
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	livePath := filepath.Join(dir, "live-lead.session.jsonl")
	liveBefore, err := os.ReadFile(livePath)
	if err != nil {
		t.Fatal(err)
	}
	h := newTestHost(t, dir)
	if _, err = h.Create(t.Context(), Options{ID: "main"}); err != nil {
		t.Fatal(err)
	}
	ops, err := store.Operations(t.Context(), "stopped-lead")
	if err != nil || len(ops) != 6 {
		t.Fatalf("operations=%+v error=%v", ops, err)
	}
	for _, op := range ops {
		want := operation.StatusCanceled
		if op.ID == operation.ID(operation.StatusCompleted) || op.ID == operation.ID(operation.StatusFailed) {
			want = operation.Status(op.ID)
		}
		if op.Status != want {
			t.Fatalf("operation %s=%s, want %s", op.ID, op.Status, want)
		}
	}
	if _, err = h.Attach("stopped-lead"); err == nil {
		t.Fatal("cleanup resumed the stopped lead")
	}
	path := filepath.Join(dir, "stopped-lead.session.jsonl")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.Create(t.Context(), Options{ID: "another"}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatal("another Open repeated cleanup", err)
	}
	liveAfter, err := os.ReadFile(livePath)
	if err != nil || string(liveBefore) != string(liveAfter) {
		t.Fatal("startup changed a live lead", err)
	}
	h.Close()
	h = newTestHost(t, dir)
	if _, err = h.Create(t.Context(), Options{ID: "restarted-main"}); err != nil {
		t.Fatal(err)
	}
	after, err = os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatal("restart duplicated terminal checkpoints", err)
	}
}

func TestFinishedSessionCleanupCancelsAbandonedChild(t *testing.T) {
	s, _, _ := newSerializedRestoreFixture(t)
	s.operations = map[operation.ID]operation.Operation{}
	s.subscribers = map[uint64]chan Event{}
	s.finish = &sessionstore.FinishRecord{Version: 1, OperationID: "finish", Result: sessionstore.FinishResult{Status: "completed", Summary: "finished"}}
	spec, err := operation.NewRemoteJobSpec(operation.RemoteJobPlan{Type: "subagent", Version: 1, Data: jsontext.Value(`{"Action":"start"}`)})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(inbox.ControlMessage{Mode: inbox.DispatchOperation, Parameters: inbox.OperationIntent{OperationID: "child", ToolName: "SubagentStart", Spec: raw}})
	if err != nil {
		t.Fatal(err)
	}
	in := inbox.Input{ID: "child", Kind: inbox.InputControl, Payload: payload}
	if err = s.store.AppendInput(t.Context(), s.ID, in); err != nil {
		t.Fatal(err)
	}
	op, err := sessionstore.DecodeOperationIntent(in)
	if err != nil {
		t.Fatal(err)
	}
	s.setOperation(op)
	if err = s.cancelStoppedChildren(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if s.operations[op.ID].Status != operation.StatusCanceled || s.Finish().Result.Summary != "finished" {
		t.Fatal("finished Session kept an unfinished child or lost its report")
	}
	ops, err := s.store.Operations(t.Context(), s.ID)
	if err != nil || len(ops) != 1 || ops[0].Status != operation.StatusCanceled {
		t.Fatalf("checkpoint=%+v error=%v", ops, err)
	}
}

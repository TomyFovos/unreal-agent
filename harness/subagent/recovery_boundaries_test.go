package subagent_test

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/projectinstructions"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	sub "github.com/unreallabsai/unreal-agent/harness/subagent"
)

// Prefix replay models loss of the process and all volatile transport state at
// a complete, fsynced record boundary. It does not simulate partial disk writes
// (covered by localfile) or claim to kill a process inside fsync.
type boundaryRecord struct {
	Type string `json:"type"`
	Data struct {
		Item      sessionstore.Item
		Operation operation.Operation
	} `json:"data"`
}

func logBoundary(t *testing.T, data []byte, after bool, match func(boundaryRecord) bool) []byte {
	t.Helper()
	offset := 0
	for _, line := range bytes.SplitAfter(data, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var r boundaryRecord
		if err := json.Unmarshal(line, &r); err != nil {
			t.Fatal(err)
		}
		if match(r) {
			if after {
				offset += len(line)
			}
			return bytes.Clone(data[:offset])
		}
		offset += len(line)
	}
	t.Fatal("requested committed boundary missing")
	return nil
}

func inputBoundary(id inbox.ID) func(boundaryRecord) bool {
	return func(r boundaryRecord) bool {
		in, ok := r.Data.Item.Data.(inbox.Input)
		return r.Type == "item" && ok && in.ID == id
	}
}

func boundaryParent(t *testing.T, dir string, mode host.Mode) (*host.Host, *host.Session, sub.Template) {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	template := sub.Template{Workspace: dir, Runtime: jsontext.Value("{}"), Policy: permission.Config{Tools: []string{"Finish", "SendParent"}}}
	model := adapter(func(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
		return llm.Response{}, nil
	})
	h, err := host.New(t.Context(), host.Config{Directory: dir, Build: factory(sub.Config{Directory: dir, Binary: binary,
		Arguments: []string{"-test.run=^TestChildServerHelper$"}, Environment: append(os.Environ(), "UNREAL_SUBAGENT_HELPER=1"),
		Templates: map[string]sub.Template{"default": template}}, model)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	bindings := projectinstructions.None()
	s, err := h.Open(t.Context(), host.Options{ID: "parent", Mode: mode, Policy: permission.Unrestricted(), ProjectInstructions: &bindings})
	if err != nil {
		t.Fatal(err)
	}
	return h, s, template
}

func TestChildFinishBeforeCommitRecovery(t *testing.T) {
	dir := t.TempDir()
	h, owner := parentAt(t, dir, "finish", host.Create)
	completed := awaitChild(t, owner, true)
	plan, err := sub.DecodePlan(completed)
	if err != nil {
		t.Fatal(err)
	}
	h.Close()
	parentLog := logBoundary(t, readLog(t, dir, owner.ID), false, func(r boundaryRecord) bool {
		return r.Type == "operation" && r.Data.Operation.ID == completed.ID && r.Data.Operation.Status == operation.StatusCompleted
	})
	childLog := logBoundary(t, readLog(t, dir, plan.ChildID), false, func(r boundaryRecord) bool {
		record, ok := r.Data.Item.Data.(sessionstore.HostRecord)
		return ok && record.Kind == "finish"
	})
	restoreLog(t, dir, owner.ID, parentLog)
	restoreLog(t, dir, plan.ChildID, childLog)
	_, resumed := parentAt(t, dir, "finish", host.Resume)
	result := awaitChild(t, resumed, true)
	if result.ID != completed.ID || result.Status != operation.StatusCompleted {
		t.Fatal(result)
	}
	view, err := sub.ReadChild(t.Context(), dir, plan.ChildID, 0, 256)
	if err != nil || view.Finish == nil {
		t.Fatal(view, err)
	}
	finishes, turns := 0, 0
	for _, item := range view.View.History.Items {
		if item.Kind == sessionstore.ItemTurn {
			turns++
		}
		if record, ok := item.Data.(sessionstore.HostRecord); ok && record.Kind == "finish" {
			finishes++
		}
	}
	if finishes != 1 || turns != 1 || countInput(t, dir, owner.ID, inbox.ID("ready:"+string(completed.ID))) != 1 {
		t.Fatal("Finish duplicated or model rerun", finishes, turns)
	}
}

func TestPersistedCancelChildRecoveryNeverRespawns(t *testing.T) {
	dir := t.TempDir()
	h, owner, template := boundaryParent(t, dir, host.Create)
	spec, _ := sub.NewSpec(sub.Plan{Version: 1, Action: "start", ParentID: owner.ID, Template: "default", Configuration: &template, Text: "wait"})
	if _, err := owner.SubmitOperation(t.Context(), owner.Generation, "spawn", "start", "SubagentStart", spec); err != nil {
		t.Fatal(err)
	}
	waitReady(t, owner)
	if _, err := owner.CancelOperation(t.Context(), owner.Generation, "cancel", "start"); err != nil {
		t.Fatal(err)
	}
	if result := awaitChild(t, owner, true); result.Status != operation.StatusCanceled {
		t.Fatal(result)
	}
	h.Close()
	childID := sub.ChildID(owner.ID, "start")
	parentLog, childLog := readLog(t, dir, owner.ID), readLog(t, dir, childID)
	for _, stage := range []struct {
		name   string
		prefix []byte
	}{
		{"intent_committed_checkpoint_missing", logBoundary(t, parentLog, true, inputBoundary("cancel"))},
		{"canceling_committed", logBoundary(t, parentLog, true, func(r boundaryRecord) bool {
			return r.Type == "operation" && r.Data.Operation.ID == "start" && r.Data.Operation.Status == operation.StatusCanceling
		})},
	} {
		t.Run(stage.name, func(t *testing.T) {
			restoreLog(t, dir, owner.ID, stage.prefix)
			h, resumed, _ := boundaryParent(t, dir, host.Resume)
			defer h.Close()
			if result := awaitChild(t, resumed, true); result.ID != "start" || result.Status != operation.StatusCanceled {
				t.Fatal(result)
			}
			if !bytes.Equal(readLog(t, dir, childID), childLog) {
				t.Fatal("canceled child executed again")
			}
			store, _ := localfile.New(dir)
			lock, err := store.AcquireWriter(childID)
			if err != nil {
				t.Fatal("canceled child has writer", err)
			}
			lock.Close()
			if _, err = resumed.CancelOperation(t.Context(), resumed.Generation, "cancel", "start"); err != nil {
				t.Fatal("cancel retry", err)
			}
			if err = sub.ResumeChild(t.Context(), resumed, sub.ControlRequest{ParentID: resumed.ID, ParentGeneration: resumed.Generation, OperationID: "start", ChildID: childID, InputID: "resume"}); err == nil {
				t.Fatal("canceled child resurrected")
			}
		})
	}
}

func TestUnconfiguredChildWithCanonicalInputFailsClosed(t *testing.T) {
	dir := t.TempDir()
	h, owner, template := boundaryParent(t, dir, host.Create)
	spec, _ := sub.NewSpec(sub.Plan{Version: 1, Action: "start", ParentID: owner.ID, Template: "default", Configuration: &template, Text: "wait"})
	if _, err := owner.SubmitOperation(t.Context(), owner.Generation, "spawn", "start", "SubagentStart", spec); err != nil {
		t.Fatal(err)
	}
	waitReady(t, owner)
	h.Close()
	childID := sub.ChildID(owner.ID, "start")
	childLog := readLog(t, dir, childID)
	prefix := logBoundary(t, childLog, false, func(r boundaryRecord) bool {
		record, ok := r.Data.Item.Data.(sessionstore.HostRecord)
		return ok && record.Kind == "configuration"
	})
	restoreLog(t, dir, childID, prefix)
	store, _ := localfile.New(dir)
	payload, _ := json.Marshal(inbox.PeerMessage{Version: 1, Sender: "parent", Target: string(childID), Handle: "start", Kind: "message", Text: "unbound history"})
	if err := store.AppendInput(t.Context(), childID, inbox.Input{ID: "unbound", Kind: inbox.InputPeer, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	before := readLog(t, dir, childID)
	h, resumed, _ := boundaryParent(t, dir, host.Resume)
	defer h.Close()
	if result := awaitChild(t, resumed, true); result.Status != operation.StatusFailed {
		t.Fatal(result)
	}
	if !bytes.Equal(readLog(t, dir, childID), before) {
		t.Fatal("unbound child history acquired and executed")
	}
}

func awaitBoundary(t *testing.T, check func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for !check() {
		select {
		case <-ctx.Done():
			t.Fatal("recovery boundary deadline")
		case <-ticker.C:
		}
	}
}

func readLog(t *testing.T, dir string, id session.ID) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, string(id)+".session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func restoreLog(t *testing.T, dir string, id session.ID, data []byte) {
	t.Helper()
	path := filepath.Join(dir, string(id)+".session.jsonl")
	if len(data) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	} else if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func countInput(t *testing.T, dir string, id session.ID, input inbox.ID) int {
	t.Helper()
	store, err := localfile.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	page, err := store.Items(t.Context(), id, 0, 256)
	if err != nil {
		t.Fatal(err)
	}
	if page.More {
		t.Fatal("fixture history exceeds assertion page")
	}
	n := 0
	for _, item := range page.Items {
		if in, ok := item.Data.(inbox.Input); ok && in.ID == input {
			n++
		}
	}
	return n
}

func TestSubagentMessageCommittedPrefixRecovery(t *testing.T) {
	dir := t.TempDir()
	h, parent, template := boundaryParent(t, dir, host.Create)
	spec, err := sub.NewSpec(sub.Plan{Version: 1, Action: "start", ParentID: parent.ID, Template: "default", Configuration: &template, Text: "wait"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = parent.SubmitOperation(t.Context(), parent.Generation, "spawn-intent", "start", "SubagentStart", spec); err != nil {
		t.Fatal(err)
	}
	op := waitReady(t, parent)
	childID := sub.ChildID(parent.ID, op.ID)
	request := sub.ControlRequest{ParentID: parent.ID, ParentGeneration: parent.Generation, OperationID: op.ID, ChildID: childID, InputID: "message", Text: "canonical payload"}
	if _, err = sub.SteerChild(t.Context(), parent, request); err != nil {
		t.Fatal(err)
	}
	var sendID operation.ID
	awaitBoundary(t, func() bool {
		v, e := parent.Inspect(0, 256)
		if e != nil {
			t.Fatal(e)
		}
		for _, o := range v.Operations {
			p, e := sub.DecodePlan(o)
			if e == nil && p.Action == "send" && o.Status == operation.StatusCompleted {
				sendID = o.ID
				return true
			}
		}
		return false
	})
	parentLog, childLog := readLog(t, dir, parent.ID), readLog(t, dir, childID)
	h.Close()
	parentBeforeSend := logBoundary(t, parentLog, false, inputBoundary("message"))
	childBeforeInput := logBoundary(t, childLog, false, inputBoundary(inbox.ID("send:"+string(sendID))))
	childAfterInput := logBoundary(t, childLog, true, inputBoundary(inbox.ID("send:"+string(sendID))))
	for _, c := range []struct {
		name          string
		parent, child []byte
	}{
		{"intent_before_commit", parentBeforeSend, childBeforeInput},
		// Sending and ACK delivery have no independent durable checkpoint.
		{"intent_after_commit_send_before_or_after_append_before", logBoundary(t, parentLog, true, inputBoundary("message")), childBeforeInput},
		{"append_after_commit_ack_before_or_after", logBoundary(t, parentLog, true, inputBoundary("message")), childAfterInput},
	} {
		t.Run(c.name, func(t *testing.T) {
			restoreLog(t, dir, "parent", c.parent)
			restoreLog(t, dir, childID, c.child)
			h, owner, _ := boundaryParent(t, dir, host.Resume)
			defer h.Close()
			request.ParentGeneration = owner.Generation
			first, err := sub.SteerChild(t.Context(), owner, request)
			if err != nil {
				t.Fatal(err)
			}
			again, err := sub.SteerChild(t.Context(), owner, request)
			if err != nil || first != again {
				t.Fatal("retry receipt", first, again, err)
			}
			changed := request
			changed.Text = "different payload"
			if _, err = sub.SteerChild(t.Context(), owner, changed); !errors.Is(err, host.ErrConflict) {
				t.Fatal("retry conflict", err)
			}
			awaitBoundary(t, func() bool {
				v, e := owner.Inspect(0, 256)
				if e != nil {
					t.Fatal(e)
				}
				for _, o := range v.Operations {
					if o.ID == sendID {
						return o.Status == operation.StatusCompleted
					}
				}
				return false
			})
			if countInput(t, dir, "parent", "message") != 1 || countInput(t, dir, childID, inbox.ID("send:"+string(sendID))) != 1 || countInput(t, dir, "parent", "ready:start") != 1 || countInput(t, dir, childID, "task:start") != 1 {
				t.Fatal("canonical input lost or duplicated")
			}
			store, _ := localfile.New(dir)
			if lock, e := store.AcquireWriter(childID); !errors.Is(e, localfile.ErrWriterOwned) {
				if lock != nil {
					lock.Close()
				}
				t.Fatal("child writer absent or duplicated", e)
			}
		})
	}
}

func TestChildToParentMessageCommittedPrefixRecovery(t *testing.T) {
	dir := t.TempDir()
	h, owner := parentAt(t, dir, "boundary-messages", host.Create)
	start := waitReady(t, owner)
	plan, err := sub.DecodePlan(start)
	if err != nil {
		t.Fatal(err)
	}
	var sendID operation.ID
	awaitBoundary(t, func() bool {
		v, e := sub.ReadChild(t.Context(), dir, plan.ChildID, 0, 256)
		if e != nil {
			return false
		}
		for _, op := range v.View.Operations {
			p, e := sub.DecodePlan(op)
			if e == nil && p.Action == "parent" && op.Status == operation.StatusCompleted {
				sendID = op.ID
				return true
			}
		}
		return false
	})
	parentLog, childLog := readLog(t, dir, owner.ID), readLog(t, dir, plan.ChildID)
	h.Close()
	intent := logBoundary(t, childLog, true, func(r boundaryRecord) bool {
		status, ok := r.Data.Item.Data.(sessionstore.ToolCallStatus)
		if !ok {
			return false
		}
		for _, id := range status.Status.WaitingFor {
			if id == sendID {
				return true
			}
		}
		return false
	})
	for _, after := range []bool{false, true} {
		t.Run(map[bool]string{false: "receiver_before_commit", true: "receiver_after_commit_lost_ack"}[after], func(t *testing.T) {
			restoreLog(t, dir, owner.ID, logBoundary(t, parentLog, after, inputBoundary(inbox.ID("send:"+string(sendID)))))
			restoreLog(t, dir, plan.ChildID, intent)
			h, resumed := parentAt(t, dir, "boundary-messages", host.Resume)
			defer h.Close()
			awaitBoundary(t, func() bool {
				v, e := sub.ReadChild(t.Context(), dir, plan.ChildID, 0, 256)
				if e != nil {
					return false
				}
				for _, op := range v.View.Operations {
					if op.ID == sendID {
						return op.Status == operation.StatusCompleted
					}
				}
				return false
			})
			v, err := resumed.Inspect(0, 256)
			if err != nil {
				t.Fatal(err)
			}
			messages := 0
			for _, item := range v.History.Items {
				if in, ok := item.Data.(inbox.Input); ok && in.Kind == inbox.InputPeer {
					p, err := in.DecodePeerMessage()
					if err != nil {
						t.Fatal(err)
					}
					if p.Text == "stable child message" {
						messages++
						if in.ID != inbox.ID("send:"+string(sendID)) || p.Sender != string(plan.ChildID) || p.Handle != string(start.ID) {
							t.Fatal("peer identity changed", in)
						}
					}
				}
			}
			if messages != 1 {
				t.Fatal("child-to-parent loss/duplicate", messages)
			}
		})
	}
}

func TestSubagentSpawnCreateReadyCommittedPrefixRecovery(t *testing.T) {
	dir := t.TempDir()
	h, parent, template := boundaryParent(t, dir, host.Create)
	spec, err := sub.NewSpec(sub.Plan{Version: 1, Action: "start", ParentID: parent.ID, Template: "default", Configuration: &template, Text: "wait"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = parent.SubmitOperation(t.Context(), parent.Generation, "spawn-intent", "start", "SubagentStart", spec); err != nil {
		t.Fatal(err)
	}
	waitReady(t, parent)
	childID := sub.ChildID(parent.ID, "start")
	parentLog, childLog := readLog(t, dir, parent.ID), readLog(t, dir, childID)
	h.Close()
	beforeReady := logBoundary(t, parentLog, false, inputBoundary("ready:start"))
	beforeSpawn := logBoundary(t, parentLog, true, inputBoundary("spawn-intent"))
	configuration := func(r boundaryRecord) bool {
		v, ok := r.Data.Item.Data.(sessionstore.HostRecord)
		return ok && v.Kind == "configuration"
	}
	for _, c := range []struct {
		name          string
		parent, child []byte
	}{
		{"spawn_before", beforeSpawn, nil},
		{"spawn_after_create_before", beforeReady, nil},
		{"create/after", beforeReady, bytes.SplitAfter(childLog, []byte{'\n'})[0]},
		{"ready/before_configuration_commit", beforeReady, logBoundary(t, childLog, false, configuration)},
		{"ready/after_configuration_commit", beforeReady, logBoundary(t, childLog, true, configuration)},
		{"initial_task/before_commit", beforeReady, logBoundary(t, childLog, false, inputBoundary("task:start"))},
		{"initial_task/after_commit", beforeReady, logBoundary(t, childLog, true, inputBoundary("task:start"))},
		{"ready_send_before_or_after_append_before", beforeReady, childLog},
		{"ready_append_after_ack_before_or_after", logBoundary(t, parentLog, true, inputBoundary("ready:start")), childLog},
	} {
		t.Run(c.name, func(t *testing.T) {
			restoreLog(t, dir, "parent", c.parent)
			restoreLog(t, dir, childID, c.child)
			h, owner, _ := boundaryParent(t, dir, host.Resume)
			defer h.Close()
			first, err := owner.SubmitOperation(t.Context(), owner.Generation, "spawn-intent", "start", "SubagentStart", spec)
			if err != nil {
				t.Fatal(err)
			}
			again, err := owner.SubmitOperation(t.Context(), owner.Generation, "spawn-intent", "start", "SubagentStart", spec)
			if err != nil || first != again {
				t.Fatal(first, again, err)
			}
			changed, _ := sub.NewSpec(sub.Plan{Version: 1, Action: "start", ParentID: owner.ID, Template: "default", Configuration: &template, Text: "changed"})
			if _, err = owner.SubmitOperation(t.Context(), owner.Generation, "spawn-intent", "start", "SubagentStart", changed); !errors.Is(err, host.ErrConflict) {
				t.Fatal(err)
			}
			recovered := waitReady(t, owner)
			plan, e := sub.DecodePlan(recovered)
			if e != nil || plan.ChildID != childID {
				t.Fatal("child identity changed", plan, e)
			}
			// waitReady can see a historic receipt before the new child opens.
			awaitBoundary(t, func() bool {
				store, _ := localfile.New(dir)
				lock, e := store.AcquireWriter(childID)
				if lock != nil {
					lock.Close()
				}
				return errors.Is(e, localfile.ErrWriterOwned)
			})
			awaitBoundary(t, func() bool { return countInput(t, dir, childID, "task:start") == 1 })
			if countInput(t, dir, "parent", "ready:start") != 1 || countInput(t, dir, "parent", "spawn-intent") != 1 {
				t.Fatal("lost or duplicate intent/ready")
			}
		})
	}
}

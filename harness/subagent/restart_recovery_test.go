package subagent_test

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	sub "github.com/unreallabsai/unreal-agent/harness/subagent"
)

// The approved behavior adds exactly one executor-loss record. All original
// bytes, tool effects, model turns, inputs and Finish expectations stay intact.
func assertRestartEndOnly(t *testing.T, dir string, id session.ID, before []byte) {
	t.Helper()
	var last time.Time
	var finished, ended bool
	for line := range bytes.SplitSeq(before, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var r boundaryRecord
		if err := json.Unmarshal(line, &r); err != nil {
			t.Fatal(err)
		}
		if r.Type == "session" {
			var header struct {
				Data struct{ Session session.Session } `json:"data"`
			}
			if err := json.Unmarshal(line, &header); err != nil {
				t.Fatal(err)
			}
			last = header.Data.Session.CreatedAt
		}
		if r.Data.Item.RecordedAt.After(last) {
			last = r.Data.Item.RecordedAt
		}
		if record, ok := r.Data.Item.Data.(sessionstore.HostRecord); ok {
			finished = finished || record.Kind == "finish"
			ended = ended || record.Kind == sessionstore.HostEnded
		}
	}
	after := readLog(t, dir, id)
	if finished || ended {
		if !bytes.Equal(before, after) {
			t.Fatal("restart changed an already ended child")
		}
		return
	}
	awaitBoundary(t, func() bool {
		after = readLog(t, dir, id)
		return len(after) != len(before) && bytes.HasSuffix(after, []byte{'\n'})
	})
	if !bytes.HasPrefix(after, before) {
		t.Fatal("restart rewrote the child's original records")
	}
	suffix := bytes.TrimSpace(after[len(before):])
	if bytes.Count(suffix, []byte{'\n'}) != 0 {
		t.Fatal("restart appended more than one record")
	}
	var record boundaryRecord
	if err := json.Unmarshal(suffix, &record); err != nil {
		t.Fatal("missing executor end", err)
	}
	end, ok := record.Data.Item.Data.(sessionstore.HostRecord)
	if record.Type != "item" || !ok || end.Kind != sessionstore.HostEnded || end.End == nil || end.End.EndedBy != "host_restart" || !end.End.EndedAt.Equal(last) {
		t.Fatal("restart appended execution or an incorrect end", record, last)
	}
}

func TestRestartRetainedChildLaterLosesExecutor(t *testing.T) {
	dir := t.TempDir()
	childHost, child, op := restartFixture(t, dir)
	h, parent, _ := boundaryParent(t, dir, host.Resume)
	defer h.Close()
	payload, _ := json.Marshal(inbox.PeerMessage{Version: 1, Sender: string(child.ID), Target: "parent", Handle: string(op.ID), Kind: "ready", Text: "reconnected"})
	if _, err := parent.SubmitPeer(t.Context(), parent.Generation, string(child.ID), string(op.ID), inbox.Input{ID: "ready:start", Kind: inbox.InputPeer, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	childHost.Close()
	before := readLog(t, dir, child.ID)
	terminal := awaitChild(t, parent, true)
	if terminal.Status != operation.StatusCanceled {
		t.Fatal(terminal)
	}
	awaitBoundary(t, func() bool { return len(endedNotices(t, parent, op.ID)) > 0 })
	if got := endedNotices(t, parent, op.ID); len(got) != 1 || !strings.Contains(got[0].Text, "ended_by=host_restart") {
		t.Fatal(got)
	}
	assertRestartEndOnly(t, dir, child.ID, before)
}

// A durable start and independently owned child model a parent process lost
// before it could commit a terminal checkpoint. The parent's old lease stays
// held until the child is running, just as it does during a real start.
func restartFixture(t *testing.T, dir string) (*host.Host, *host.Session, operation.Operation) {
	t.Helper()
	store, err := localfile.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Create(t.Context(), "parent"); err != nil {
		t.Fatal(err)
	}
	lease, err := store.AcquireWriter("parent")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	template := sub.Template{Workspace: dir, Runtime: jsontext.Value(`{}`), Policy: permission.Config{Tools: []string{"Finish", "SendParent"}}}
	spec, err := sub.NewSpec(sub.Plan{Version: 1, Action: "start", ParentID: "parent", Template: "default", Configuration: &template, Text: "continue existing work", Notify: true})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(spec)
	payload, _ := json.Marshal(inbox.ControlMessage{Mode: inbox.DispatchOperation, Parameters: inbox.OperationIntent{OperationID: "start", ToolName: "SubagentStart", Spec: raw}})
	if err = store.AppendInput(t.Context(), "parent", inbox.Input{ID: "start-intent", Kind: inbox.InputControl, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	op := operation.Operation{ID: "start", ToolName: "SubagentStart", Type: spec.Type, Version: spec.Version, State: spec.State, MaxOutputLength: spec.MaxOutputLength, Status: operation.StatusAwaiting}
	if err = store.SaveOperation(t.Context(), "parent", op); err != nil {
		t.Fatal(err)
	}
	p, err := sub.DecodePlan(op)
	if err != nil {
		t.Fatal(err)
	}
	c := sub.ChildConfig{Version: 1, ParentID: "parent", ChildID: p.ChildID, OperationID: op.ID, SessionDirectory: dir, Workspace: dir, Runtime: template.Runtime, Policy: template.Policy, ReadyID: "ready:start", Task: p.Text}
	configuration, _ := json.Marshal(c)
	childHost, err := host.New(t.Context(), host.Config{Directory: dir, Build: factory(sub.Config{Directory: dir, Child: &c}, adapter(func(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
		return response("Finish", `{"status":"completed","summary":"existing child finished","changedFiles":[],"tests":[],"blockers":[]}`), nil
	}))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { childHost.Close() })
	child, err := childHost.Create(t.Context(), host.Options{ID: p.ChildID, Lifecycle: "child", Configuration: configuration, Policy: permission.Unrestricted()})
	if err != nil {
		t.Fatal(err)
	}
	return childHost, child, op
}

func endedNotices(t *testing.T, parent *host.Session, handle operation.ID) []inbox.PeerMessage {
	t.Helper()
	v, err := parent.Inspect(0, 256)
	if err != nil {
		t.Fatal(err)
	}
	var notices []inbox.PeerMessage
	for _, item := range v.History.Items {
		if in, ok := item.Data.(inbox.Input); ok && in.Kind == inbox.InputPeer {
			peer, _ := in.DecodePeerMessage()
			if peer.Kind == "ended" && peer.Handle == string(handle) {
				notices = append(notices, peer)
			}
		}
	}
	return notices
}

func TestRestartOrphanRecordsLastTimeAndNotifiesOnce(t *testing.T) {
	dir := t.TempDir()
	childHost, child, op := restartFixture(t, dir)
	childHost.Close() // no Finish or terminal parent checkpoint
	before := readLog(t, dir, child.ID)
	for range 2 {
		h, parent, _ := boundaryParent(t, dir, host.Resume)
		terminal := awaitChild(t, parent, true)
		if terminal.ID != op.ID || terminal.Status != operation.StatusCanceled {
			t.Fatal(terminal)
		}
		awaitBoundary(t, func() bool { return len(endedNotices(t, parent, op.ID)) > 0 })
		notices := endedNotices(t, parent, op.ID)
		if len(notices) != 1 || !strings.Contains(notices[0].Text, "ended_by=host_restart") || !strings.Contains(notices[0].Text, "Start it again") {
			t.Fatal(notices)
		}
		assertRestartEndOnly(t, dir, child.ID, before)
		v, err := sub.ReadChildSnapshot(t.Context(), dir, child.ID)
		if err != nil || v.End == nil || v.Finish != nil {
			t.Fatal(v.End, v.Finish, err)
		}
		h.Close()
	}
}

func TestRestartRetainsLiveChildThatReconnects(t *testing.T) {
	dir := t.TempDir()
	_, child, op := restartFixture(t, dir)
	h, parent, _ := boundaryParent(t, dir, host.Resume)
	defer h.Close()
	// The reconnect uses the new parent generation and the original child/start
	// identities. Its existing writer remains held throughout the handshake.
	payload, _ := json.Marshal(inbox.PeerMessage{Version: 1, Sender: string(child.ID), Target: "parent", Handle: string(op.ID), Kind: "ready", Text: "existing child reconnected"})
	if _, err := parent.SubmitPeer(t.Context(), parent.Generation, string(child.ID), string(op.ID), inbox.Input{ID: "ready:start", Kind: inbox.InputPeer, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	v, err := parent.Inspect(0, 256)
	if err != nil {
		t.Fatal(err)
	}
	for _, current := range v.Operations {
		if current.ID == op.ID && current.Status != operation.StatusAwaiting {
			t.Fatal("live child was ended", current)
		}
	}
	if len(endedNotices(t, parent, op.ID)) != 0 {
		t.Fatal("live child received an ended notice")
	}
	before, err := sub.ReadChildSnapshot(t.Context(), dir, child.ID)
	if err != nil || before.End != nil || before.Finish != nil {
		t.Fatal("live child was reconciled", before.End, err)
	}
	// Its original runtime, rather than a newly spawned process, commits Finish.
	task, _ := json.Marshal("finish the original task")
	if _, err := child.Submit(t.Context(), child.Generation, inbox.Input{ID: "continue", Kind: inbox.InputExternal, Payload: task}); err != nil {
		t.Fatal(err)
	}
	terminal := awaitChild(t, parent, true)
	if terminal.ID != op.ID || terminal.Status != operation.StatusCompleted {
		t.Fatal(terminal)
	}
	after, err := sub.ReadChildSnapshot(t.Context(), dir, child.ID)
	if err != nil || after.End != nil || after.Finish == nil {
		t.Fatal("live completion lost", after.End, after.Finish, err)
	}
	for _, item := range v.History.Items {
		if r, ok := item.Data.(sessionstore.HostRecord); ok && r.Activity != nil && r.Activity.Kind == "child_start" {
			t.Fatal("spawned a duplicate child")
		}
	}
}

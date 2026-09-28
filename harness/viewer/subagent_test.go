package viewer

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/subagent"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"sync"
	"testing"
	"time"
)

func startSpec(t *testing.T, parent session.ID, dir string) operation.Spec {
	t.Helper()
	spec, err := subagent.NewSpec(subagent.Plan{Version: 1, Action: "start", ParentID: parent, Template: "default", Text: "inspect task", Configuration: &subagent.Template{Workspace: dir, Runtime: jsontext.Value("{}")}})
	must(t, err)
	return spec
}
func opFromSpec(id operation.ID, s operation.Spec) operation.Operation {
	return operation.Operation{ID: id, Type: s.Type, Version: s.Version, State: s.State, MaxOutputLength: s.MaxOutputLength, Status: operation.StatusAwaiting, ToolName: "SubagentStart"}
}
func TestActualSubagentDecoderCanonicalFinishAndUnsupportedPlans(t *testing.T) {
	op := opFromSpec("parent-start", startSpec(t, "parent", t.TempDir()))
	child, ok, err := DecodeSubagent("parent", op)
	must(t, err)
	if !ok || child.ID != subagent.ChildID("parent", op.ID) {
		t.Fatal("child identity")
	}
	if _, _, err = DecodeSubagent("spoof", op); err == nil {
		t.Fatal("spoofed parent accepted")
	}
	v := view("parent", "g", 1)
	v.Operations = []operation.Operation{op}
	m := New(SubagentOptions())
	must(t, m.Replace("parent", v))
	finish := sessionstore.Item{Sequence: 1, RecordedAt: epoch.Add(time.Second), Kind: sessionstore.ItemHostRecord, Data: sessionstore.HostRecord{Version: 1, Kind: "finish", Finish: &sessionstore.FinishRecord{Version: 1, OperationID: "child-finish", Result: sessionstore.FinishResult{Status: "completed", Summary: "child's explicit report", Tests: []string{"unit tests"}}}}}
	must(t, m.Replace(child.ID, view(child.ID, "", 0, finish)))
	got := row(t, m, child.ID)
	if got.Finish == nil || got.Finish.OperationID != "child-finish" || got.ParentOperationID != "parent-start" || got.Runtime != RuntimeUnknown {
		t.Fatalf("%+v", got)
	}
	// Parent Handle.Finish is a cache; it cannot provide the child's canonical result.
	m2 := New(SubagentOptions())
	state, err := operation.DecodeRemoteJobState(op)
	must(t, err)
	state.Handle, _ = json.Marshal(subagent.Handle{Version: 1, ParentID: "parent", ChildID: child.ID, OperationID: op.ID, Finish: &sessionstore.FinishRecord{Version: 1, OperationID: "cached", Result: sessionstore.FinishResult{Status: "completed", Summary: "cached report"}}})
	step, err := operation.UpdateRemoteJob(op, state, operation.StatusCompleted)
	must(t, err)
	v.Operations = []operation.Operation{*step.Operation}
	must(t, m2.Replace("parent", v))
	if row(t, m2, child.ID).Finish != nil {
		t.Fatal("cached Handle became canonical Finish")
	}
	send, err := subagent.NewSpec(subagent.Plan{Version: 1, Action: "send", ParentID: "parent", Handle: "parent-start", Text: "hello"})
	must(t, err)
	if _, ok, err = DecodeSubagent("parent", opFromSpec("send-op", send)); err != nil || ok {
		t.Fatal("send created another child", err)
	}
	state.Plan.Version = 99
	raw, _ := json.Marshal(state)
	op.State = raw
	if _, _, err = DecodeSubagent("parent", op); err == nil {
		t.Fatal("unknown plan version accepted")
	}
}

// This manager deliberately leaves submitted operations awaiting. It lets tests
// verify actual Host persistence/control boundaries without launching executors.
type heldManager struct {
	mu      sync.Mutex
	values  map[operation.ID]operation.Operation
	updates chan operation.Operation
}

func newHeldManager() *heldManager {
	return &heldManager{values: map[operation.ID]operation.Operation{}, updates: make(chan operation.Operation, 32)}
}
func (m *heldManager) Add(op operation.Operation) error {
	m.mu.Lock()
	m.values[op.ID] = op
	m.mu.Unlock()
	return nil
}
func (m *heldManager) Updates() <-chan operation.Operation { return m.updates }
func (m *heldManager) Cancel(id operation.ID, _ string) error {
	m.mu.Lock()
	op, ok := m.values[id]
	m.mu.Unlock()
	if ok {
		op.Status = operation.StatusCanceled
		m.updates <- op
	}
	return nil
}
func viewerOwner(t *testing.T, dir string) *host.Host {
	t.Helper()
	h, err := host.New(t.Context(), host.Config{Directory: dir, Build: func(ctx context.Context, _ session.ID) (host.Runtime, error) {
		return host.Runtime{Builder: contextbuilder.NewBuilder(), LLM: &viewerLLM{}, Tools: tool.NewRegistry(tool.StaticTranslators{}), Operations: newHeldManager()}, nil
	}})
	must(t, err)
	t.Cleanup(func() { h.Close() })
	return h
}
func TestHostAdapterPersistsControlsAndChildReadDoesNotAcquireWriter(t *testing.T) {
	dir := t.TempDir()
	h := viewerOwner(t, dir)
	parent, err := h.Create(t.Context(), host.Options{ID: "parent", Lifecycle: "interactive", Policy: permission.Unrestricted()})
	must(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	spec := startSpec(t, parent.ID, dir)
	_, err = parent.SubmitOperation(ctx, parent.Generation, "start-request", "start-op", "SubagentStart", spec)
	must(t, err)
	childID := subagent.ChildID(parent.ID, "start-op")
	config := subagent.ChildConfig{Version: 1, ParentID: parent.ID, ChildID: childID, OperationID: "start-op", SessionDirectory: dir, Workspace: dir, Runtime: jsontext.Value("{}"), ReadyID: "ready:start-op", Task: "inspect task"}
	raw, err := json.Marshal(config)
	must(t, err)
	child, err := h.Create(ctx, host.Options{ID: childID, Lifecycle: "child", Configuration: raw, Policy: permission.Unrestricted()})
	must(t, err)
	reader := ParentReader{Host: h, ParentID: parent.ID, Directory: dir}
	read, err := reader.Inspect(ctx, childID, 0, 4096)
	must(t, err)
	if read.Generation != "" || read.Running || read.Session.Session.ID != child.ID {
		t.Fatal("read invented liveness")
	}
	if _, err = reader.Inspect(ctx, "unrelated", 0, 10); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	// Repeated reads succeed while the child writer remains held by the same Host.
	c := NewClient(reader, New(SubagentOptions()), HostControls{Host: h})
	must(t, c.Refresh(ctx, parent.ID))
	must(t, c.Refresh(ctx, childID))
	receipt, err := c.Steer(ctx, childID, "steer-request", "continue carefully")
	must(t, err)
	again, err := HostControls{Host: h}.SteerChild(ctx, ControlRequest{ParentID: parent.ID, ParentGeneration: parent.Generation, OperationID: "start-op", ChildID: childID, InputID: "steer-request", Text: "continue carefully"})
	must(t, err)
	if receipt != again {
		t.Fatal("stable intent receipt changed")
	}
	p, err := parent.Inspect(0, 256)
	must(t, err)
	inputCount := 0
	sendCount := 0
	for _, item := range p.History.Items {
		if input, ok := item.Data.(inbox.Input); ok && input.ID == "steer-request" {
			inputCount++
		}
	}
	for _, op := range p.Operations {
		plan, err := subagent.DecodePlan(op)
		if err == nil && plan.Action == "send" {
			sendCount++
			if plan.Handle != "start-op" || plan.Text != "continue carefully" {
				t.Fatal("wrong persisted Send plan")
			}
		}
	}
	if inputCount != 1 || sendCount != 1 {
		t.Fatalf("persisted %d inputs/%d sends", inputCount, sendCount)
	}
	for i := range 5 {
		must(t, c.Resume(ctx, childID, fmt.Sprintf("resume-%d", i)))
	}
	same, err := h.Attach(childID)
	must(t, err)
	if same.Generation != child.Generation {
		t.Fatal("viewer resumed a second child")
	}
	// The backend still validates stale generation and parent identity even when
	// called without the frontend's friendly checks.
	bad := ControlRequest{ParentID: parent.ID, ParentGeneration: "old", OperationID: "start-op", ChildID: childID, InputID: "bad"}
	if err = (HostControls{Host: h}).ResumeChild(ctx, bad); !errors.Is(err, host.ErrStaleGeneration) {
		t.Fatal(err)
	}
	bad.ParentGeneration = parent.Generation
	bad.ChildID = "unrelated"
	if _, err = (HostControls{Host: h}).CancelChild(ctx, bad); err == nil {
		t.Fatal("wrong child cancellation")
	}
	_, err = c.Cancel(ctx, childID, "cancel-request", "user canceled")
	must(t, err)
	page, err := parent.Inspect(receipt.Sequence, 256)
	must(t, err)
	found := false
	for _, item := range page.History.Items {
		if input, ok := item.Data.(inbox.Input); ok && input.ID == "cancel-request" {
			control, err := input.DecodeControlMessage()
			must(t, err)
			found = control.Mode == inbox.CancelOperation
		}
	}
	if !found {
		t.Fatal("cancel bypassed canonical Inbox")
	}
}

package viewer

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

type topologyReader struct {
	mu        sync.Mutex
	views     map[session.ID]host.View
	inspected []session.ID
}

func (r *topologyReader) Inspect(_ context.Context, id session.ID, after sessionstore.Sequence, limit int) (host.View, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inspected = append(r.inspected, id)
	v, ok := r.views[id]
	if !ok {
		return host.View{}, ErrUnavailable
	}
	if after != 0 || limit < 1 {
		return host.View{}, ErrInvalidPage
	}
	return copyValue(v)
}
func (*topologyReader) Subscribe(context.Context, session.ID, sessionstore.Sequence, int, int) (host.Subscription, error) {
	return host.Subscription{}, ErrUnavailable
}

func TestPanelTopologyObservationBoundedScopedAndReadOnly(t *testing.T) {
	m := New(Options{DecodeChild: childDecoder, DecodeFinish: DecodeFinish})
	parent := view("parent", "g", 1)
	r := &topologyReader{views: map[session.ID]host.View{}}
	for i := range 20 {
		id := session.ID(fmt.Sprintf("child-%02d", i))
		parent.Operations = append(parent.Operations, operation.Operation{ID: operation.ID(fmt.Sprint(i)), Type: "child", ToolName: string(id), Status: operation.StatusReady})
		call := llm.ToolCall{CallID: "call", Name: "read", Arguments: `{"path":"child.go","private":"argument-never-shown"}`}
		op := operation.Operation{ID: "read", ToolName: "read", Status: operation.StatusReady}
		child := view(id, "", 0,
			host.HistoryItem{Sequence: 1, Kind: sessionstore.ItemModelResponse, Data: sessionstore.ModelResponse{Response: llm.Response{Output: []llm.Item{{Type: llm.ItemToolCall, Data: call}}}}},
			host.HistoryItem{Sequence: 2, Kind: sessionstore.ItemToolCallStatus, Data: sessionstore.ToolCallStatus{CallID: "call", Operations: []operation.Operation{op}}})
		child.Operations = []operation.Operation{op}
		r.views[id] = child
	}
	must(t, m.Replace("parent", parent))
	controls := &fakeControls{}
	c := NewClient(r, m, controls)
	p := &Panel{client: c, parent: "parent", ctx: t.Context(), updates: make(chan struct{}, 1)}
	before := append([]operation.Operation(nil), parent.Operations...)
	if len(p.refreshTargets()) != 0 {
		t.Fatal("Chat began observing unselected children")
	}
	p.SetTopologyVisible(true)
	seen := map[session.ID]bool{}
	for range 3 {
		targets := p.refreshTargets()
		if len(targets) > 8 {
			t.Fatal("unbounded child read batch", targets)
		}
		for _, id := range targets {
			seen[id] = true
		}
	}
	if len(seen) != 20 {
		t.Fatal("round-robin observation starved children", len(seen))
	}
	p.refreshChildren()
	if len(r.inspected) != 8 {
		t.Fatal("unexpected topology read count", r.inspected)
	}
	for _, id := range r.inspected {
		child := row(t, m, id)
		if len(child.Operations) != 1 || child.Operations[0].Target != "child.go" {
			t.Fatal("child operations not projected", child.Operations)
		}
	}
	if !reflect.DeepEqual(parent.Operations, before) || len(controls.calls) != 0 || m.Selected() != "parent" {
		t.Fatal("topology reads created/control-owned work")
	}
	// A nested plan becomes visible through its parent's canonical snapshot.
	// Observation does not widen the existing direct-child reader authority.
	id := r.inspected[0]
	child := r.views[id]
	child.Operations = append(child.Operations, operation.Operation{ID: "spawn-nested", Type: "child", ToolName: "grandchild", Status: operation.StatusReady})
	must(t, m.Replace(id, child))
	if row(t, m, "grandchild").Depth != 2 {
		t.Fatal("nested metadata not exposed")
	}
	for _, target := range p.refreshTargets() {
		if target == "grandchild" {
			t.Fatal("observation widened gateway authorization")
		}
	}
	must(t, m.Select("child-00"))
	p.SetTopologyVisible(false)
	targets := p.refreshTargets()
	if len(targets) != 1 || targets[0] != "child-00" {
		t.Fatal("Chat lost existing selected-child observation", targets)
	}
}

func TestPanelTopologyRefreshObservesCanonicalFinishWithoutControls(t *testing.T) {
	m := New(Options{DecodeChild: childDecoder, DecodeFinish: DecodeFinish})
	must(t, m.Replace("parent", parentView(operation.StatusReady)))
	r := &topologyReader{views: map[session.ID]host.View{"child": view("child", "", 0, input(1, "public child input"))}}
	controls := &fakeControls{}
	p := &Panel{client: NewClient(r, m, controls), parent: "parent", ctx: t.Context(), updates: make(chan struct{}, 1)}
	p.SetTopologyVisible(true)
	p.refreshChildren()
	if row(t, m, "child").Finish != nil {
		t.Fatal("Finish inferred before canonical commit")
	}
	r.views["child"] = view("child", "", 0, input(1, "public child input"), host.HistoryItem{Sequence: 2, RecordedAt: epoch.Add(time.Second), Kind: sessionstore.ItemHostRecord, Data: sessionstore.HostRecord{Version: 1, Kind: "finish", Finish: &sessionstore.FinishRecord{Version: 1, OperationID: "finish", Result: sessionstore.FinishResult{Status: "completed", Summary: "public final report"}}}})
	p.refreshChildren()
	if f := row(t, m, "child").Finish; f == nil || f.Status != "completed" {
		t.Fatal("canonical Finish not observed")
	}
	if len(p.refreshTargets()) != 0 || len(controls.calls) != 0 {
		t.Fatal("terminal child continued polling or controls executed")
	}
	for _, n := range p.Snapshot(time.Now()).Rows {
		for _, op := range n.Operations {
			if strings.Contains(op.Target, "public child input") {
				t.Fatal("prompt used as an operation target")
			}
		}
	}
}

package host

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

// Keep full-view concurrent Inspect coverage independent of the Structured
// Bridge's event-driven wait. Notifications must follow persistence, snapshots
// must be monotonic/isolated, and a fresh snapshot must match the durable Store.
func TestConcurrentInspectSubscribeCanonicalConsistency(t *testing.T) {
	started := make(chan struct{})
	h, err := New(t.Context(), Config{Directory: t.TempDir(), Build: factory(modelFunc(func(ctx context.Context, _ llm.Request, _ llm.RequestOptions) (llm.Response, error) {
		close(started)
		<-ctx.Done()
		return llm.Response{}, ctx.Err()
	}))})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	s, err := h.Create(t.Context(), Options{ID: "observations"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := timeout(t)
	receipt, err := s.Submit(ctx, s.Generation, input("start", "hold the synthetic model"))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("model did not reach the observation barrier")
	}
	v, err := s.Inspect(0, 4096)
	if err != nil {
		t.Fatal(err)
	}
	var turn session.TurnID
	for _, item := range v.History.Items {
		if item.Kind == sessionstore.ItemTurn {
			turn = item.Data.(session.Turn).ID
		}
	}
	if turn == "" {
		t.Fatal("missing canonical turn")
	}
	store := &serializedStore{s}
	op := operation.Operation{ID: "observed-operation", Type: "test", Version: 1, Status: operation.StatusAwaiting, State: jsontext.Value(`{"step":0}`)}
	if err := store.AppendToolCallStatus(ctx, s.ID, sessionstore.ToolCallStatus{TurnID: turn, CallID: "observed-call", Status: tool.CallStatus{WaitingFor: []operation.ID{op.ID}}, Operations: []operation.Operation{op}}); err != nil {
		t.Fatal(err)
	}
	sub, err := s.Subscribe(0, 4096, 128)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Cancel()
	jobs := make(chan struct{})
	inspected := make(chan error, 1)
	go func() {
		var revision uint64
		var cursor sessionstore.Sequence
		for range jobs {
			view, e := s.Inspect(0, 4096)
			if e == nil && (view.Generation != s.Generation || view.Revision < revision || view.History.NextAfter < cursor || len(view.Operations) != 1) {
				e = errors.New("concurrent inspect regressed or lost operation")
			}
			if e == nil {
				for i, item := range view.History.Items {
					if item.Sequence != sessionstore.Sequence(i+1) {
						e = errors.New("concurrent inspect contains a torn history")
					}
				}
				// Mutating a returned operation must not change another view,
				// subscription payload, or canonical persisted operation.
				view.Operations[0].State[0] = '!'
				revision, cursor = view.Revision, view.History.NextAfter
			}
			inspected <- e
		}
	}()
	defer close(jobs)
	for step := 1; step <= 24; step++ {
		jobs <- struct{}{} // Inspect overlaps the next post-commit publication.
		op.State = jsontext.Value(fmt.Sprintf(`{"step":%d}`, step))
		if err := store.SaveOperation(ctx, s.ID, op); err != nil {
			t.Fatal(err)
		}
		if err := <-inspected; err != nil {
			t.Fatal(err)
		}
		for {
			select {
			case event, open := <-sub.Events:
				if !open || event.Kind == "gap" {
					t.Fatal("concurrent subscriber lost a committed update")
				}
				if event.Kind != "operation" {
					continue
				}
				current, err := s.Inspect(0, 4096)
				if err != nil || event.Revision > current.Revision || event.Generation != current.Generation || event.Operation == nil || !reflect.DeepEqual(*event.Operation, op) || len(current.Operations) != 1 || !reflect.DeepEqual(current.Operations[0], op) {
					t.Fatal("event and canonical inspect disagree", err)
				}
				s.mu.Lock()
				persisted, err := s.store.Operations(ctx, s.ID)
				s.mu.Unlock()
				if err != nil || len(persisted) != 1 || !reflect.DeepEqual(persisted[0], op) {
					t.Fatal("notification preceded its durable operation", err)
				}
				event.Operation.State[0] = '!'
			case <-ctx.Done():
				t.Fatal("concurrent publication did not complete")
			}
			break
		}
	}
	again, err := s.Submit(ctx, s.Generation, input("start", "hold the synthetic model"))
	if err != nil || again != receipt {
		t.Fatal("observations changed idempotent delivery", err)
	}
	final, err := s.Inspect(0, 4096)
	if err != nil {
		t.Fatal(err)
	}
	page, err := store.Items(ctx, s.ID, 0, 4096)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(final.History)
	want, _ := json.Marshal(ProjectHistoryPage(page))
	if string(got) != string(want) || len(final.Operations) != 1 || !reflect.DeepEqual(final.Operations[0], op) {
		t.Fatal("final inspect lost durable history or operation")
	}
	if !reflect.DeepEqual(sub.Initial.Operations[0].State, jsontext.Value(`{"step":0}`)) {
		t.Fatal("initial subscription snapshot was mutated by later publications")
	}
}

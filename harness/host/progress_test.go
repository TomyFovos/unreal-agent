package host

import (
	"context"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func progressSession() *Session {
	return &Session{running: true, Generation: "g", subscribers: map[uint64]chan Event{}, items: []sessionstore.Item{{Kind: sessionstore.ItemTurn, Data: session.Turn{ID: "turn1"}}}}
}
func TestProgressBoundResetLateAndFallback(t *testing.T) {
	s := progressSession()
	var late func(llm.Progress)
	a := s.withProgress(modelFunc(func(ctx context.Context, r llm.Request, o llm.RequestOptions) (llm.Response, error) {
		late = o.Progress
		o.Progress(llm.Progress{Attempt: 1, Delta: strings.Repeat("猫", 30000)})
		v, _ := s.Inspect(0, 1)
		if v.Progress == nil || len(v.Progress.Text) > maxProgressBytes || !v.Progress.Truncated {
			t.Fatal("unbounded progress")
		}
		o.Progress(llm.Progress{Attempt: 2, Reset: true})
		o.Progress(llm.Progress{Attempt: 1, Delta: "stale attempt"})
		o.Progress(llm.Progress{Attempt: 2, Delta: "current"})
		return llm.Response{}, nil
	}))
	if _, err := a.Respond(t.Context(), llm.Request{}, llm.RequestOptions{}); err != nil {
		t.Fatal(err)
	}
	late(llm.Progress{Attempt: 3, Delta: "late"})
	v, _ := s.Inspect(0, 1)
	if v.Progress.Text != "current" || !v.Progress.Done {
		t.Fatalf("late progress: %+v", v.Progress)
	}
	s.mu.Lock()
	s.items = append(s.items, sessionstore.Item{Kind: sessionstore.ItemTurn, Data: session.Turn{ID: "turn2"}})
	s.mu.Unlock()
	fallback := s.withProgress(modelFunc(func(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
		late(llm.Progress{Attempt: 99, Delta: "old turn"})
		return llm.Response{}, nil
	}))
	fallback.Respond(t.Context(), llm.Request{}, llm.RequestOptions{})
	v, _ = s.Inspect(0, 1)
	if v.Progress.Mode != "completed_response" || v.Progress.Text != "" || v.Progress.TurnID != "turn2" {
		t.Fatalf("fallback: %+v", v.Progress)
	}
	s.mu.Lock()
	s.rememberItem(sessionstore.Item{Kind: sessionstore.ItemModelResponse, Data: sessionstore.ModelResponse{TurnID: "turn2"}}, false)
	s.mu.Unlock()
	v, _ = s.Inspect(0, 1)
	if v.Progress != nil {
		t.Fatal("completed response did not replace temporary progress")
	}
}
func TestProgressCanceledAndSlowSubscriber(t *testing.T) {
	s := progressSession()
	sub, err := s.Subscribe(0, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Cancel()
	ctx, cancel := context.WithCancel(t.Context())
	a := s.withProgress(modelFunc(func(_ context.Context, _ llm.Request, o llm.RequestOptions) (llm.Response, error) {
		o.Progress(llm.Progress{Attempt: 1, Delta: "before"})
		cancel()
		for range 10000 {
			o.Progress(llm.Progress{Attempt: 1, Delta: "after"})
		}
		return llm.Response{}, ctx.Err()
	}))
	a.Respond(ctx, llm.Request{}, llm.RequestOptions{})
	v, _ := s.Inspect(0, 1)
	if v.Progress.Text != "before" {
		t.Fatal("canceled deltas visible")
	}
	if e := <-sub.Events; e.Kind != "gap" {
		t.Fatalf("slow subscriber: %s", e.Kind)
	}
}

func TestCanceledOldInvocationCannotReplaceNewProgress(t *testing.T) {
	s := progressSession()
	s.progressEpoch = 2
	s.progress = &Progress{TurnID: "turn2", Epoch: 2, Text: "new"}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	called := false
	old := s.withProgress(modelFunc(func(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
		called = true
		return llm.Response{}, nil
	}))
	if _, err := old.Respond(ctx, llm.Request{}, llm.RequestOptions{}); err == nil || called {
		t.Fatal("canceled invocation ran")
	}
	v, _ := s.Inspect(0, 1)
	if v.Progress.Text != "new" || v.Progress.Epoch != 2 {
		t.Fatal("late scheduling overwrote current progress")
	}
}

func TestProgressToolContinuationGetsItsOwnEphemeralEpoch(t *testing.T) {
	s := progressSession()
	a := s.withProgress(modelFunc(func(ctx context.Context, _ llm.Request, o llm.RequestOptions) (llm.Response, error) {
		o.Progress(llm.Progress{Attempt: 1, Delta: "old draft"})
		first, _ := s.Inspect(0, 1)
		if _, err := o.Tools(ctx, llm.Response{}); err != nil {
			t.Fatal(err)
		}
		o.Progress(llm.Progress{Attempt: 1, Delta: "new draft"})
		v, _ := s.Inspect(0, 10)
		if v.Progress == nil || v.Progress.TurnID != "turn2" || v.Progress.Epoch <= first.Progress.Epoch || v.Progress.Text != "new draft" {
			t.Fatal("continuation dropped streaming or retained a canonicalized draft", v.Progress)
		}
		return llm.Response{}, nil
	}))
	_, err := a.Respond(t.Context(), llm.Request{}, llm.RequestOptions{Tools: func(context.Context, llm.Response) ([]llm.ToolOutcome, error) {
		s.mu.Lock()
		s.rememberItem(sessionstore.Item{Kind: sessionstore.ItemModelResponse, Data: sessionstore.ModelResponse{TurnID: "turn1"}}, false)
		s.rememberItem(sessionstore.Item{Kind: sessionstore.ItemTurn, Data: session.Turn{ID: "turn2", PreviousTurnID: "turn1", ToolContinuation: "turn1"}}, false)
		s.mu.Unlock()
		return nil, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
}

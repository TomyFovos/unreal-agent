package host

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
)

func TestInitialBatchPrecedesFirstModelDecision(t *testing.T) {
	const count = 64
	var calls atomic.Int32
	model := modelFunc(func(ctx context.Context, request llm.Request, opts llm.RequestOptions) (llm.Response, error) {
		calls.Add(1)
		users := 0
		for _, item := range request.Input {
			if message, ok := item.Data.(llm.Message); ok && message.Role == llm.RoleUser {
				users++
			}
		}
		if users != count {
			return llm.Response{}, fmt.Errorf("model saw %d initial inputs, want %d", users, count)
		}
		return echo(ctx, request, opts)
	})
	h, err := New(t.Context(), Config{Directory: t.TempDir(), Build: factory(model)})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	initial := make([]inbox.Input, 0, count+2)
	for i := range count {
		initial = append(initial, input(fmt.Sprint(i), "message"))
	}
	initial = append(initial, initial[0])
	payload, _ := json.Marshal(inbox.ControlMessage{Mode: inbox.StopWhenIdle})
	initial = append(initial, inbox.Input{ID: "stop", Kind: inbox.InputControl, Payload: payload})
	s, err := h.Create(t.Context(), Options{ID: "initial", Initial: initial})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Wait(timeout(t)); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("model calls = %d", calls.Load())
	}
	receipt, err := s.Submit(t.Context(), s.Generation, initial[0])
	if err != nil || receipt.Sequence == 0 {
		t.Fatal(receipt, err)
	}
	if _, err = h.Resume(t.Context(), Options{ID: s.ID, Initial: []inbox.Input{input("0", "conflict")}}); err != ErrConflict {
		t.Fatal(err)
	}
}

func TestInitialHardStopDoesNotCallModel(t *testing.T) {
	var calls atomic.Int32
	h, err := New(t.Context(), Config{Directory: t.TempDir(), Build: factory(modelFunc(func(ctx context.Context, r llm.Request, o llm.RequestOptions) (llm.Response, error) {
		calls.Add(1)
		return echo(ctx, r, o)
	}))})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	payload, _ := json.Marshal(inbox.ControlMessage{Mode: inbox.StopHard})
	s, err := h.Create(t.Context(), Options{ID: "hard", Initial: []inbox.Input{input("message", "hello"), {ID: "stop", Kind: inbox.InputControl, Payload: payload}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Wait(timeout(t)); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("model called before initial hard stop")
	}
}

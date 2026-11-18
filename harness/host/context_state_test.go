package host

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/contextstate"
	"github.com/unreallabsai/unreal-agent/harness/llm"
)

func TestHostContextStateRestoresWithoutProjection(t *testing.T) {
	ctx := t.Context()
	requests := make(chan llm.Request, 2)
	h, err := New(ctx, Config{Directory: t.TempDir(), Build: factory(modelFunc(func(ctx context.Context, r llm.Request, opt llm.RequestOptions) (llm.Response, error) {
		requests <- r
		return echo(ctx, r, opt)
	}))})
	checkHostRestore(t, err)
	defer h.Close()
	s, err := h.Create(ctx, Options{ID: "state"})
	checkHostRestore(t, err)
	const rule = "context state fixture with a distinctive rule"
	event, err := contextstate.NewEvent("rule", contextstate.RuleRecorded, contextstate.Rule{ID: "rule", Excerpt: rule})
	checkHostRestore(t, err)
	loc, err := s.AppendContextEvent(ctx, event)
	checkHostRestore(t, err)
	state, err := s.ContextState(ctx)
	checkHostRestore(t, err)
	expected, err := json.Marshal(state)
	checkHostRestore(t, err)
	current := state.Rules["rule"]
	current.Sources[0] = "mutated-caller"
	delete(state.Rules, "rule")
	stop(t, s)

	s, err = h.Resume(ctx, Options{ID: "state"})
	checkHostRestore(t, err)
	fromRestore, err := s.ContextState(ctx)
	checkHostRestore(t, err)
	encoded, err := json.Marshal(fromRestore)
	checkHostRestore(t, err)
	if !bytes.Equal(encoded, expected) || fromRestore.Rules["rule"].Sources[0] != loc {
		t.Fatalf("restored state changed: %s; expected %s", encoded, expected)
	}
	page, err := s.RecordShow(ctx, loc, 0)
	checkHostRestore(t, err)
	if !bytes.Contains(page.Bytes, []byte(rule)) {
		t.Fatalf("rule event locator did not resolve: %s", page.Bytes)
	}
	_, err = s.Submit(ctx, s.Generation, input("user", "continue"))
	checkHostRestore(t, err)
	select {
	case req := <-requests:
		wire, err := json.Marshal(req)
		checkHostRestore(t, err)
		if bytes.Contains(wire, []byte(rule)) || !bytes.Contains(wire, []byte("continue")) {
			t.Fatalf("model request changed by L2: %s", wire)
		}
	case <-timeout(t).Done():
		t.Fatal("resumed Session did not send its user input")
	}
}

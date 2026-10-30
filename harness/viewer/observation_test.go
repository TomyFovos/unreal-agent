package viewer

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
)

type observationCursorReader struct {
	cursorReader
	observations  int
	appendBetween bool
}

func (r *observationCursorReader) Observe(context.Context, session.ID, int) (host.View, error) {
	r.observations++
	v, err := copyValue(r.current)
	if r.appendBetween {
		r.current.History.Items = append(r.current.History.Items, response(2, "between seed and subscribe", 20, 3))
		r.current.History.NextAfter = 2
		r.appendBetween = false
	}
	return v, err
}

func TestObservationWatchSubscribesFromAtomicCursorAndCatchesAppend(t *testing.T) {
	r := &observationCursorReader{cursorReader: cursorReader{current: view("s", "g", 1, response(1, "old", 10, 2))}, appendBetween: true}
	r.current.Running = false
	c := NewClient(r, nil, nil)
	must(t, c.Watch(t.Context(), "s", nil))
	got := row(t, c.Model, "s")
	if r.observations != 1 || !reflect.DeepEqual(r.subscribed, []sessionstore.Sequence{1}) || len(r.inspected) != 0 || got.Cursor != 2 || got.Usage.Input != 30 {
		t.Fatal("bootstrap lost or replayed history", got, r.subscribed, r.inspected)
	}
	r.current = view("s", "new-generation", 2, response(1, "new", 40, 4))
	r.current.Running = false
	must(t, c.Watch(t.Context(), "s", nil))
	if r.observations != 2 || row(t, c.Model, "s").Usage.Input != 40 {
		t.Fatal("generation switch did not rebuild observation")
	}
}

func TestObservationRefreshMatchesFullHistoryAndKeepsOriginalHistory(t *testing.T) {
	dir := t.TempDir()
	store, err := localfile.New(dir)
	must(t, err)
	_, err = store.Create(t.Context(), "observed")
	must(t, err)
	var previous session.TurnID
	for i := range 90 {
		id := session.TurnID(strings.Repeat("t", i+1))
		text := strings.Repeat("a", 159) + "👨‍👩‍👧‍👦" + strings.Repeat(" old full body ", 100)
		must(t, store.AppendInput(t.Context(), "observed", inbox.Input{ID: inbox.ID(id), Kind: inbox.InputExternal, Payload: []byte(`"input"`)}))
		must(t, store.AppendTurn(t.Context(), "observed", session.Turn{ID: id, PreviousTurnID: previous}))
		must(t, store.AppendModelResponse(t.Context(), "observed", sessionstore.ModelResponse{TurnID: id, Response: llm.Response{ID: string(id), Model: "model", Usage: llm.Usage{InputTokens: 10, CachedInputTokens: 5, OutputTokens: 2}, Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: text}}}}}))
		previous = id
	}
	h := viewerOwner(t, dir)
	s, err := h.Resume(t.Context(), host.Options{ID: "observed"})
	must(t, err)
	full, err := s.Inspect(0, 4096)
	must(t, err)
	for _, recent := range []int{1, 128, 256} {
		want := New(Options{RecentLimit: recent})
		must(t, want.Replace(s.ID, full))
		got := New(Options{RecentLimit: recent})
		reader := ParentReader{Host: h, ParentID: s.ID, Directory: dir}
		c := NewClient(reader, got, nil)
		must(t, c.Refresh(t.Context(), s.ID))
		if !reflect.DeepEqual(want.Rows(epoch), got.Rows(epoch)) {
			t.Fatal("metadata observation changed viewer rows", recent, want.Rows(epoch), got.Rows(epoch))
		}
		a, _ := want.Detail(s.ID, epoch)
		b, _ := got.Detail(s.ID, epoch)
		if !reflect.DeepEqual(a.Recent, b.Recent) {
			t.Fatal("recent transcript changed", recent)
		}
		page, err := c.History(t.Context(), s.ID, 0, 128)
		must(t, err)
		if !strings.Contains(page.Items[2].Data.(sessionstore.ModelResponse).Response.Output[0].Data.(llm.Message).Text, "old full body") {
			t.Fatal("explicit history returned abbreviated text")
		}
	}
}

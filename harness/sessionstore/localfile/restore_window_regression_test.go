package localfile

import (
	"context"
	"encoding/json/jsontext"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

type p3Window interface {
	Items(context.Context, session.ID, sessionstore.Sequence, int) (sessionstore.Page, error)
	Close() error
}

func TestRestoreReadWindowRejectsAppendBetweenPages(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := session.ID("read-window")
	owner, err := s.AcquireWriter(id)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if _, err = s.Create(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"one", "two", "three", "four"} {
		if err = s.AppendInput(t.Context(), id, inbox.Input{ID: inbox.ID(name), Kind: inbox.InputExternal, Payload: jsontext.Value(`{"message":"synthetic"}`)}); err != nil {
			t.Fatal(err)
		}
	}
	view, err := s.OpenRestoreView(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()
	first, err := view.Items(t.Context(), id, 0, 2)
	if err != nil || len(first.Items) != 2 || !first.More {
		t.Fatalf("first page %v %v", first, err)
	}
	if err = s.AppendInput(t.Context(), id, inbox.Input{ID: "five", Kind: inbox.InputExternal, Payload: jsontext.Value(`{"message":"new canonical boundary"}`)}); err != nil {
		t.Fatal(err)
	}
	second, err := view.Items(t.Context(), id, first.NextAfter, 100)
	if err == nil {
		t.Fatalf("canonical append silently mixed restore boundaries: old first page=2, second page=%d, last=%d", len(second.Items), second.NextAfter)
	}
	if err.Error() != "restore read view changed" {
		t.Fatalf("unexpected classification: %v", err)
	}
}

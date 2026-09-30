package viewer

import (
	"context"

	"errors"
	"github.com/rivo/uniseg"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"strings"
	"testing"
	"time"
)

func TestPanelSelectionPagingAndStableRetry(t *testing.T) {
	controls := &fakeControls{}
	c := controlClient(t, controls)
	child := view("child", "", 0, input(1, "saved-message"))
	child.History.More = true
	next := view("child", "", 0, input(2, "second"))
	c.Reader = &fakeReader{pages: map[sessionstore.Sequence]host.View{0: child, 1: next}}
	p := &Panel{client: c, parent: "parent", updates: make(chan struct{}, 1)}
	text, ok := p.Command(t.Context(), "/child child")
	if !ok || !strings.Contains(text, "Selected") || c.Model.Selected() != "child" {
		t.Fatal(text)
	}
	for _, command := range []string{"/child-history 0 1", "/child-next"} {
		text, ok = p.Command(t.Context(), command)
		if !ok || !strings.Contains(text, "History next=") {
			t.Fatal(text)
		}
	}
	if p.transcript.NextAfter != 2 {
		t.Fatal("page cursor did not advance")
	}
	controls.err = errors.New("connection lost")
	text, ok = p.Command(t.Context(), "/child-send ask parent")
	if !ok || !strings.Contains(text, "/child-retry") || len(controls.calls) != 1 {
		t.Fatal(text)
	}
	controls.err = nil
	text, ok = p.Command(t.Context(), "/child-retry")
	if !ok || !strings.Contains(text, "committed") || len(controls.calls) != 2 || controls.calls[0] != controls.calls[1] {
		t.Fatal("retry changed input", text)
	}
	rendered := p.Render(100, 40)
	if !strings.Contains(rendered, "runtime: unknown") || !strings.Contains(rendered, "History next=2") {
		t.Fatal(rendered)
	}
	for _, line := range strings.Split(p.Render(24, 12), "\n") {
		if len([]rune(line)) > 24 {
			t.Fatal("panel exceeded width")
		}
	}
	if _, ok = p.Command(t.Context(), "/unrelated"); ok {
		t.Fatal("panel consumed unrelated command")
	}
}
func TestPanelCloseJoinsObservationOnly(t *testing.T) {
	reader := &fakeReader{subscriptions: []host.Subscription{{Initial: view("parent", "g", 0), Events: make(chan host.Event)}}}
	client := NewClient(reader, New(Options{}), nil)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	p := NewPanel(ctx, "parent", client)
	select {
	case <-p.Updates():
	case <-ctx.Done():
		t.Fatal("no initial panel update")
	}
	p.Close()
	reader.mu.Lock()
	calls, canceled := reader.subscribed, reader.canceled
	reader.mu.Unlock()
	if calls != 1 || canceled != 1 {
		t.Fatal("panel leaked subscription")
	}
	if row(t, client.Model, "parent").Runtime != RuntimeUnknown {
		t.Fatal("closed panel claimed live ownership")
	}
}
func TestTransientProgressNeverAddsUsageOrCanonicalActivity(t *testing.T) {
	m := New(Options{})
	must(t, m.Replace("s", view("s", "g", 0)))
	for i := uint64(1); i <= 3; i++ {
		must(t, m.Apply("s", host.Event{Generation: "g", Revision: i, Kind: "progress", Progress: &host.Progress{Epoch: 1, Attempt: i, Text: "draft which is retried"}}))
	}
	got := row(t, m, "s")
	if got.NeedsResync || got.Activity != "" || got.Usage.Known || got.Cursor != 0 {
		t.Fatalf("%+v", got)
	}
	must(t, m.Apply("s", host.Event{Generation: "g", Revision: 4, Kind: "item", Item: ptr(response(1, "final", 10, 2))}))
	if row(t, m, "s").Usage.Input != 10 {
		t.Fatal("final accounting missing")
	}
}

func TestPanelTerminalColumnBounds(t *testing.T) {
	for _, text := range []string{"日本語の表示", "👩‍💻🙂👨‍👩‍👧‍👦", "ééé"} {
		for _, width := range []int{1, 2, 3, 8} {
			clipped := clipLine(text, width)
			if uniseg.StringWidth(clipped) > width || !strings.HasPrefix(text, clipped) {
				t.Fatalf("width=%d text=%q clipped=%q", width, text, clipped)
			}
			if strings.HasSuffix(clipped, "e") || strings.HasSuffix(clipped, "\u200d") {
				t.Fatalf("split grapheme: %q", clipped)
			}
		}
	}
	c := controlClient(t, &fakeControls{})
	p := &Panel{client: c, parent: "parent", updates: make(chan struct{}, 1)}
	for _, width := range []int{1, 2, 24} {
		for _, line := range strings.Split(p.Render(width, 12), "\n") {
			if uniseg.StringWidth(line) > width {
				t.Fatalf("panel width=%d line=%q", width, line)
			}
		}
	}
	empty := &Panel{client: NewClient(nil, New(Options{}), nil)}
	if uniseg.StringWidth(empty.Render(2, 1)) > 2 {
		t.Fatal("loading panel exceeded width")
	}
}

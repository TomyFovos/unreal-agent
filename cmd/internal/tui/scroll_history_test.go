package tui

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func TestLongCanonicalResponseAllVisualLinesReachable(t *testing.T) {
	text := "FIRST RESPONSE LINE\n```go\n"
	for i := range 220 {
		text += fmt.Sprintf("// response line %03d 日本語 👩🏽‍💻 é\n", i)
	}
	text += "```\nLAST RESPONSE LINE"
	entries := entriesFor(host.HistoryItem{Sequence: 1, Kind: sessionstore.ItemModelResponse, Data: sessionstore.ModelResponse{TurnID: "long", Response: llm.Response{Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: text}}}}}})
	if len(entries) != 1 || entries[0].Clipped {
		t.Fatal("ordinary long response was clipped before scrolling")
	}
	for _, size := range [][2]int{{44, 14}, {60, 20}, {80, 24}, {130, 32}} {
		s := Snapshot{ID: "long", Connected: true, Running: true, Entries: entries}
		u := UIState{Theme: Theme{Plain: true}}
		bottom := RenderFrame(s, u, size[0], size[1])
		if !strings.Contains(frameText(bottom), "LAST RESPONSE LINE") {
			t.Fatal("missing last line", size)
		}
		u.Scroll = bottom.ScrollMax
		top := RenderFrame(s, u, size[0], size[1])
		if !strings.Contains(frameText(top), "FIRST RESPONSE LINE") {
			t.Fatal("missing first line", size)
		}
		if bottom.ConversationRows < strings.Count(text, "\n")+1 {
			t.Fatal("wrapped lines absent from scroll range", size)
		}
		var viewed strings.Builder
		for scroll := 0; ; scroll = min(bottom.ScrollMax, scroll+max(1, bottom.ConversationHeight)) {
			u.Scroll = scroll
			viewed.WriteString(frameText(RenderFrame(s, u, size[0], size[1])))
			if scroll == bottom.ScrollMax {
				break
			}
		}
		for i := range 220 {
			if !strings.Contains(viewed.String(), fmt.Sprintf("response line %03d", i)) {
				t.Fatalf("page traversal skipped visual line %d at %v", i, size)
			}
		}
	}
}

type pagedTranscriptClient struct {
	*fakeClient
	pageSize int
}

func (f *pagedTranscriptClient) Inspect(_ context.Context, _ session.ID, after sessionstore.Sequence, limit int) (host.View, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inspects++
	v := f.view
	end := min(len(v.History.Items), int(after)+min(limit, f.pageSize))
	if int(after) > end {
		return host.View{}, errors.New("cursor out of range")
	}
	v.History.Items = append([]host.HistoryItem(nil), v.History.Items[int(after):end]...)
	v.History.NextAfter = sessionstore.Sequence(end)
	v.History.More = end < len(f.view.History.Items)
	return v, nil
}
func (f *pagedTranscriptClient) Subscribe(ctx context.Context, id session.ID, after sessionstore.Sequence, limit, _ int) (host.Subscription, error) {
	v, err := f.Inspect(ctx, id, after, limit)
	return host.Subscription{Initial: v, Events: f.events, Cancel: func() {}}, err
}

func conversationFixture(texts ...string) *pagedTranscriptClient {
	f := &pagedTranscriptClient{fakeClient: newFake(), pageSize: 2}
	add := func(kind sessionstore.ItemKind, data any) {
		f.view.History.Items = append(f.view.History.Items, host.HistoryItem{Sequence: sessionstore.Sequence(len(f.view.History.Items) + 1), Kind: kind, Data: data})
	}
	for i, text := range texts {
		payload, _ := json.Marshal("public user input")
		add(sessionstore.ItemInput, inbox.Input{Kind: inbox.InputExternal, Payload: payload})
		turn := session.TurnID(fmt.Sprintf("turn-%d", i+1))
		add(sessionstore.ItemTurn, session.Turn{ID: turn, Type: session.TurnRegular})
		add(sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: turn, Response: llm.Response{Output: []llm.Item{
			{Type: llm.ItemReasoning, Data: llm.Reasoning{Summary: []string{"hidden private reasoning"}}},
			{Type: llm.ItemMessage, ProviderID: "private-provider-state", Data: llm.Message{Role: llm.RoleAssistant, Text: text}},
			{Type: llm.ItemToolCall, Data: llm.ToolCall{Name: "private tool", Arguments: "private tool arguments"}},
		}}})
	}
	return f
}

var transcriptNumber = regexp.MustCompile(`entry-(\d+)`)

func visibleEntryRange(out *screenObserver) (int, int) {
	lo, hi := 100000, -1
	for _, match := range transcriptNumber.FindAllStringSubmatch(out.text(), -1) {
		n, _ := strconv.Atoi(match[1])
		lo = min(lo, n)
		hi = max(hi, n)
	}
	return lo, hi
}

func TestTranscriptPagesBeyondLiveDisplayWindowAndReturnToFollow(t *testing.T) {
	f := &pagedTranscriptClient{fakeClient: newFake(), pageSize: 128}
	for i := range 1300 {
		f.view.History.Items = append(f.view.History.Items, host.HistoryItem{Sequence: sessionstore.Sequence(i + 1), Kind: sessionstore.ItemModelResponse, Data: sessionstore.ModelResponse{Response: llm.Response{Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: fmt.Sprintf("entry-%04d 日本語", i)}}}}}})
	}
	// Metadata after the last rendered response must not trap PgDn on an old
	// page forever, or cause a false "no later messages" failure.
	f.view.History.Items = append(f.view.History.Items, host.HistoryItem{Sequence: 1301, Kind: sessionstore.ItemTurn, Data: session.Turn{ID: "metadata-only-tail"}})
	keys := make(chan Key, 8)
	out := &screenObserver{}
	done := make(chan error, 1)
	theme := Theme{Plain: true}
	go func() { done <- Run(t.Context(), Config{Client: f, ID: "s", Keys: keys, Output: out, Theme: &theme}) }()
	defer func() {
		keys <- Key{Name: "detach"}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}()
	waitFor(t, func() bool { _, hi := visibleEntryRange(out); return hi == 1299 })
	for i := 0; i < 200; i++ {
		lo, _ := visibleEntryRange(out)
		if lo == 0 {
			break
		}
		keys <- Key{Name: "pageup"}
		waitFor(t, func() bool { n, _ := visibleEntryRange(out); return n < lo })
	}
	if lo, _ := visibleEntryRange(out); lo != 0 {
		t.Fatal("oldest transcript unreachable", lo)
	}
	for i := 0; i < 200; i++ {
		_, hi := visibleEntryRange(out)
		if hi == 1299 && !strings.Contains(out.text(), "scrollback:") {
			break
		}
		keys <- Key{Name: "pagedown"}
		waitFor(t, func() bool {
			_, n := visibleEntryRange(out)
			return n > hi || n == 1299 && !strings.Contains(out.text(), "scrollback:")
		})
	}
	if _, hi := visibleEntryRange(out); hi != 1299 || strings.Contains(out.text(), "scrollback:") {
		t.Fatal("did not return to live follow", hi)
	}
}

func TestScrollResizeAndModalSeparation(t *testing.T) {
	f := conversationFixture("FIRST\n" + strings.Repeat("canonical 日本語 line which wraps on a narrow display\n", 100) + "LATEST")
	keys := make(chan Key, 16)
	out := &screenObserver{}
	done := make(chan error, 1)
	theme := Theme{Plain: true}
	var width atomic.Int64
	width.Store(80)
	go func() {
		done <- Run(t.Context(), Config{Client: f, ID: "s", Keys: keys, Output: out, Theme: &theme, Size: func() (int, int) { return int(width.Load()), 24 }})
	}()
	defer func() {
		keys <- Key{Name: "detach"}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}()
	waitFor(t, func() bool { return strings.Contains(out.text(), "LATEST") })
	keys <- Key{Name: "pageup"}
	waitFor(t, func() bool { return strings.Contains(out.text(), "scrollback:") })
	hint := regexp.MustCompile(`scrollback: (\d+) lines`).FindStringSubmatch(out.text())
	width.Store(60)
	waitFor(t, func() bool {
		return strings.Contains(out.text(), "scrollback: "+hint[1]+" lines") && out.cursorX() < 60
	})
	keys <- Key{Text: "/analyze"}
	keys <- Key{Name: "enter"}
	waitFor(t, func() bool { return strings.Contains(out.text(), "Analyze") })
	keys <- Key{Name: "down"}
	keys <- Key{Name: "enter"}
	waitFor(t, func() bool { return strings.Contains(out.text(), "Analysis") })
	keys <- Key{Name: "pagedown"}
	keys <- Key{Name: "escape"}
	keys <- Key{Name: "escape"}
	waitFor(t, func() bool { return strings.Contains(out.text(), "scrollback: "+hint[1]+" lines") })
	keys <- Key{Text: "new user prompt"}
	keys <- Key{Name: "enter"}
	waitFor(t, func() bool { return !strings.Contains(out.text(), "scrollback:") })
}

func TestScrollDoesNotJumpOnStreamingAndCanonicalReplacement(t *testing.T) {
	var lines strings.Builder
	for i := range 100 {
		fmt.Fprintf(&lines, "entry-%04d initial canonical\n", i)
	}
	f := conversationFixture(lines.String())
	keys := make(chan Key, 16)
	out := &screenObserver{}
	done := make(chan error, 1)
	theme := Theme{Plain: true}
	go func() { done <- Run(t.Context(), Config{Client: f, ID: "s", Keys: keys, Output: out, Theme: &theme}) }()
	defer func() {
		keys <- Key{Name: "detach"}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}()
	waitFor(t, func() bool { _, hi := visibleEntryRange(out); return hi == 99 })
	keys <- Key{Name: "pageup"}
	waitFor(t, func() bool { return strings.Contains(out.text(), "scrollback:") })
	lo, hi := visibleEntryRange(out)
	progress := &host.Progress{Epoch: 1, TurnID: "next", Mode: "streaming", Text: strings.Repeat("streaming draft line\n", 40)}
	f.events <- host.Event{Generation: "g", Revision: 1, Kind: "progress", Progress: progress}
	waitFor(t, func() bool { return strings.Contains(out.text(), "generating") })
	// The pulse takes one row. The first visible original line stays anchored;
	// the last row may move by that one row rather than jumping to the draft.
	if a, b := visibleEntryRange(out); a != lo || b != hi-1 {
		t.Fatal("streaming moved the scroll anchor", lo, hi, a, b)
	}
	f.mu.Lock()
	f.view.Revision = 2
	f.view.History.Items = append(f.view.History.Items, host.HistoryItem{Sequence: 4, Kind: sessionstore.ItemModelResponse, Data: sessionstore.ModelResponse{TurnID: "next", Response: llm.Response{Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: strings.Repeat("canonical completion line\n", 40)}}}}}})
	f.view.Progress = &host.Progress{Epoch: 1, TurnID: "next", Mode: "complete", Done: true}
	f.mu.Unlock()
	f.events <- host.Event{Generation: "g", Revision: 2, Kind: "history"}
	waitFor(t, func() bool { return !strings.Contains(out.text(), "generating") })
	if a, b := visibleEntryRange(out); a != lo || b != hi {
		t.Fatal("canonical replacement moved the scroll anchor", lo, hi, a, b)
	}
	keys <- Key{Text: "/help"}
	keys <- Key{Name: "enter"}
	waitFor(t, func() bool { return strings.Contains(out.text(), "Type / to search") })
	keys <- Key{Name: "pageup"}
	keys <- Key{Name: "escape"}
	waitFor(t, func() bool {
		return strings.Contains(out.text(), "scrollback:") && !strings.Contains(out.text(), "Type / to search")
	})
	if a, b := visibleEntryRange(out); a != lo || b != hi {
		t.Fatal("help page navigation changed transcript")
	}
}

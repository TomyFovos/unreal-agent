package tui

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
)

type lazyHistoryClient struct {
	*pagedTranscriptClient
	backwards, backwardItems int
	gate                     <-chan struct{}
	entered                  chan<- struct{}
}

func (f *lazyHistoryClient) InspectBefore(ctx context.Context, _ session.ID, before sessionstore.Sequence, limit int) (host.View, error) {
	if before > 0 && f.gate != nil {
		if f.entered != nil {
			f.entered <- struct{}{}
		}
		select {
		case <-f.gate:
		case <-ctx.Done():
			return host.View{}, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	end := len(f.view.History.Items)
	if before > 0 {
		end = int(before - 1)
	}
	start := max(0, end-limit)
	v := f.view
	v.History.Items = append([]host.HistoryItem(nil), v.History.Items[start:end]...)
	v.History.NextAfter = sessionstore.Sequence(end)
	v.History.More = end < len(f.view.History.Items)
	v.History.Older = start > 0
	f.backwards++
	f.backwardItems += len(v.History.Items)
	return v, nil
}
func lazyFixture(n int) *lazyHistoryClient {
	f := &lazyHistoryClient{pagedTranscriptClient: &pagedTranscriptClient{fakeClient: newFake(), pageSize: 128}}
	for i := range n {
		f.view.History.Items = append(f.view.History.Items, host.HistoryItem{Sequence: sessionstore.Sequence(i + 1), Kind: sessionstore.ItemModelResponse, Data: sessionstore.ModelResponse{Response: llm.Response{Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: fmt.Sprintf("entry-%04d", i)}}}, Usage: llm.Usage{InputTokens: 1, OutputTokens: 2}}}})
	}
	return f
}
func TestLazyHistoryTailPrependOrderAndFullConsumers(t *testing.T) {
	f := lazyFixture(3000)
	m := NewModel("s")
	v, err := f.InspectBefore(t.Context(), "s", 0, historyPageItems)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.seedTail(v); err != nil {
		t.Fatal(err)
	}
	s := m.Snapshot()
	if len(s.Entries) != historyPageItems || s.After != 3000 || s.HistoryFirst != 2873 || !s.OlderDropped || f.backwardItems != historyPageItems {
		t.Fatalf("initial tail: %d %+v", len(s.Entries), s.HistoryFirst)
	}
	if err = m.EnsureAnalysis(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	full := NewModel("s")
	if err = full.apply(f.view); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	a, b := m.Analysis(now, nil), full.Analysis(now, nil)
	if !m.analysisFull || a.Partial != b.Partial || a.Usage.Input != b.Usage.Input || a.Usage.Output != b.Usage.Output {
		t.Fatalf("analysis not complete: %+v vs %+v", a.Usage, b.Usage)
	}
	choices, err := CopyChoices(t.Context(), f, "s", s.After)
	if err != nil {
		t.Fatal(err)
	}
	text, err := CopyMessagesText(t.Context(), f, "s", choices)
	if err != nil {
		t.Fatal(err)
	}
	path, err := ExportConversation(t.Context(), f, "s", s.After, filepath.Join(t.TempDir(), "exports"))
	if err != nil {
		t.Fatal(err)
	}
	exported, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{text, string(exported)} {
		if !strings.Contains(body, "entry-0000") || !strings.Contains(body, "entry-2999") || strings.Count(body, "entry-") != 3000 {
			t.Fatal("full consumer omitted messages")
		}
	}
	after, revision := s.After, s.Revision
	for m.Snapshot().OlderDropped {
		if err = m.LoadOlder(t.Context(), f); err != nil {
			t.Fatal(err)
		}
	}
	s = m.Snapshot()
	if len(s.Entries) != 3000 || s.After != after || s.Revision != revision {
		t.Fatal("prepend changed cursor or lost entries")
	}
	for i, e := range s.Entries {
		if e.Sequence != sessionstore.Sequence(i+1) {
			t.Fatalf("wrong order at %d: %d", i, e.Sequence)
		}
	}
	u := UIState{HistoryTop: true, Theme: Theme{Plain: true}}
	bottom := RenderFrame(s, u, 80, 24)
	u.Scroll = bottom.ScrollMax
	if !strings.Contains(frameText(RenderFrame(s, u, 80, 24)), "ここが会話の始まり") {
		t.Fatal("missing beginning")
	}
}

func TestLazyHistoryWatchReconnectAndGap(t *testing.T) {
	f := lazyFixture(3000)
	m := NewModel("s")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Watch(ctx, f, m, func() {}) }()
	defer func() { cancel(); <-done }()
	waitFor(t, func() bool { return m.Snapshot().After == 3000 })
	f.mu.Lock()
	count, items := f.backwards, f.backwardItems
	f.mu.Unlock()
	if count != 1 || items != 128 || len(m.Snapshot().Entries) != 128 {
		t.Fatal("watch replayed full history", count, items)
	}
	if err := m.LoadOlder(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	// Force a new Host generation and a gap while appending beyond the tail.
	f.mu.Lock()
	f.view.Generation = "g2"
	f.view.Revision = 5
	for i := 3000; i < 3010; i++ {
		f.view.History.Items = append(f.view.History.Items, host.HistoryItem{Sequence: sessionstore.Sequence(i + 1), Kind: sessionstore.ItemModelResponse, Data: sessionstore.ModelResponse{Response: llm.Response{Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Text: fmt.Sprintf("entry-%04d", i)}}}}}})
	}
	f.mu.Unlock()
	f.events <- host.Event{Kind: "gap"}
	waitFor(t, func() bool { s := m.Snapshot(); return s.Generation == "g2" && s.After == 3010 })
	s := m.Snapshot()
	if len(s.Entries) != 266 {
		t.Fatal("reconnect changed loaded prefix", len(s.Entries))
	}
	for i, e := range s.Entries {
		if e.Sequence != sessionstore.Sequence(2745+i) {
			t.Fatalf("duplicate/gap at %d: %d", i, e.Sequence)
		}
	}
	f.mu.Lock()
	count = f.backwards
	f.mu.Unlock()
	if count != 2 {
		t.Fatal("reconnect reloaded tail", count)
	}
}

func TestLazyHistoryScrollLoadsAndShowsLoading(t *testing.T) {
	f := lazyFixture(260)
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	f.gate = gate
	f.entered = entered
	keys := make(chan Key, 4)
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
	waitFor(t, func() bool { return strings.Contains(out.text(), "entry-0259") })
	for i := 0; i < 40; i++ {
		keys <- Key{Name: "pageup"}
		select {
		case <-entered:
			goto loading
		case <-time.After(5 * time.Millisecond):
		}
	}
	t.Fatal("PgUp never requested older history")
loading:
	waitFor(t, func() bool { return strings.Contains(out.text(), "古い履歴を読み込み中") })
	close(gate)
	waitFor(t, func() bool { return !strings.Contains(out.text(), "古い履歴を読み込み中") })
	for i := 0; i < 50; i++ {
		keys <- Key{Name: "up"}
		if strings.Contains(out.text(), "ここが会話の始まり") {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Page upwards by screens after exercising the single-line trigger.
	for i := 0; i < 50; i++ {
		keys <- Key{Name: "pageup"}
		if strings.Contains(out.text(), "ここが会話の始まり") {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("beginning unreachable")
}

// Measure the complete frontend attach projection plus its first frame. Both
// cases use identical 10 MB canonical fixtures and fresh caches each iteration.
func BenchmarkHistoryStartup(b *testing.B) {
	f := lazyFixture(4000)
	for i := range f.view.History.Items {
		d := f.view.History.Items[i].Data.(sessionstore.ModelResponse)
		msg := d.Response.Output[0].Data.(llm.Message)
		msg.Text += " " + strings.Repeat("synthetic conversation ", 110)
		d.Response.Output[0].Data = msg
		f.view.History.Items[i].Data = d
	}
	for _, lazy := range []bool{false, true} {
		name := "full"
		if lazy {
			name = "tail128"
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				m := NewModel("s")
				if lazy {
					v, err := f.InspectBefore(b.Context(), "s", 0, historyPageItems)
					if err != nil {
						b.Fatal(err)
					}
					if err = m.seedTail(v); err != nil {
						b.Fatal(err)
					}
				} else {
					if err := m.apply(f.view); err != nil {
						b.Fatal(err)
					}
				}
				benchmarkFrame = RenderFrame(m.Snapshot(), UIState{Theme: Theme{Plain: true}, cache: &bodyCache{}}, 80, 24)
			}
		})
	}
}

func TestLazyHistorySummaryKeepsBackgroundTopologyAndReceipts(t *testing.T) {
	f := lazyFixture(3000)
	at := time.Unix(1000, 0)
	payload, _ := json.Marshal("public input")
	f.view.History.Items[0] = host.HistoryItem{Sequence: 1, Kind: sessionstore.ItemHostRecord, Data: sessionstore.HostRecord{Version: 1, Kind: "configuration", Configuration: []byte(`{"Workspace":"/synthetic","Provider":{"provider":"claude-code","model":{"id":"m"}}}`)}}
	f.view.History.Items[1] = host.HistoryItem{Sequence: 2, Kind: sessionstore.ItemInput, RecordedAt: at, Data: inbox.Input{Kind: inbox.InputExternal, Payload: payload}}
	peer, _ := json.Marshal(inbox.PeerMessage{Version: 1, Sender: "child", Target: "s", Handle: "worker", Kind: "ended", Text: "background"})
	f.view.History.Items[2] = host.HistoryItem{Sequence: 3, Kind: sessionstore.ItemInput, Data: inbox.Input{Kind: inbox.InputPeer, Payload: peer}}
	full := NewModel("s")
	if err := full.apply(f.view); err != nil {
		t.Fatal(err)
	}
	baseline := full.Snapshot()
	v, err := f.InspectBefore(t.Context(), "s", 0, 128)
	if err != nil {
		t.Fatal(err)
	}
	v.History.Background = true
	v.HistoryState = &host.HistoryState{Configuration: []byte(`{"Workspace":"/synthetic"}`), Selection: baseline.Selection, TurnID: baseline.TurnID, LatestKind: baseline.LatestKind, LatestAt: baseline.LatestAt, LastUserAt: baseline.LastUserAt, Background: true, CacheLatest: llm.Usage{InputTokens: 1, OutputTokens: 2}, CacheTotal: llm.Usage{InputTokens: 2997, OutputTokens: 5994, Raw: []byte(`{}`)}, UsageResponses: 2997}
	m := NewModel("s")
	if err = m.seedTail(v); err != nil {
		t.Fatal(err)
	}
	s := m.Snapshot()
	if !backgroundTurn(s) || s.Workspace != "/synthetic" || !reflect.DeepEqual(s.Selection, baseline.Selection) || !reflect.DeepEqual(s.CacheTotal, baseline.CacheTotal) {
		t.Fatal("tail lost current state")
	}
	u := UIState{Now: at.Add(time.Hour), Viewer: viewer.PanelSnapshot{ParentID: "s", Rows: []viewer.Row{{ID: "child", ParentID: "s", CreatedAt: at.Add(-time.Hour), ParentOperationStatus: operation.StatusFailed, Runtime: viewer.RuntimeStopped, Finish: &viewer.Finish{Status: "failure", RecordedAt: at.Add(-time.Minute)}}}}}
	cards, beads := buildWorkCards(s, u.Viewer, u.Now)
	// Removing transcript rows cannot change child retention, marks or attention.
	empty := s
	empty.Entries = nil
	otherCards, otherBeads := buildWorkCards(empty, u.Viewer, u.Now)
	if !reflect.DeepEqual(cards, otherCards) || !reflect.DeepEqual(beads, otherBeads) || !reflect.DeepEqual(marksFor(u, s), marksFor(u, empty)) || !reflect.DeepEqual(attentionBand(s, u, makeLayout(120, 32)), attentionBand(empty, u, makeLayout(120, 32))) {
		t.Fatal("topology depends on loaded transcript")
	}
	text := chatText(s, u, 120)
	if strings.Contains(text, "entry-") || !strings.Contains(text, "裏") {
		t.Fatal("omitted peer prefix exposed background text", text)
	}
	// A status beyond an older page must restore its chip linkage and elapsed time.
	entries := entriesFor(host.HistoryItem{Sequence: 1, Kind: sessionstore.ItemModelResponse, Data: sessionstore.ModelResponse{Response: llm.Response{Output: []llm.Item{{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "call", Name: "Read"}}}}}})
	applyHistoryReceipts(entries, []host.HistoryReceipt{{Status: sessionstore.ToolCallStatus{CallID: "call", Operations: []operation.Operation{{ID: "op", ToolName: "Read", Status: operation.StatusCompleted}}}, StartedAt: at, At: at.Add(time.Second)}})
	if entries[0].Calls[0].Status != operation.StatusCompleted || entries[0].Calls[0].Operations[0] != "op" || !entries[0].Calls[0].StartedAt.Equal(at) {
		t.Fatal("old receipt not restored")
	}
}

func TestLazyHistoryHomeReachesBeginning(t *testing.T) {
	f := lazyFixture(400)
	keys := make(chan Key, 4)
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
	waitFor(t, func() bool { return strings.Contains(out.text(), "entry-0399") })
	keys <- Key{Name: "historyhome"}
	waitFor(t, func() bool {
		return strings.Contains(out.text(), "ここが会話の始まり") && strings.Contains(out.text(), "entry-0000")
	})
	keys <- Key{Name: "historyend"}
	waitFor(t, func() bool {
		return strings.Contains(out.text(), "entry-0399") && !strings.Contains(out.text(), "scrollback:")
	})
}

func BenchmarkHistoryFirstFrame(b *testing.B) {
	f := lazyFixture(4000)
	for i := range f.view.History.Items {
		d := f.view.History.Items[i].Data.(sessionstore.ModelResponse)
		msg := d.Response.Output[0].Data.(llm.Message)
		msg.Text += " " + strings.Repeat("synthetic conversation ", 110)
		d.Response.Output[0].Data = msg
		f.view.History.Items[i].Data = d
	}
	for _, lazy := range []bool{false, true} {
		name := "full"
		if lazy {
			name = "tail128"
		}
		m := NewModel("s")
		if lazy {
			v, err := f.InspectBefore(b.Context(), "s", 0, 128)
			if err != nil {
				b.Fatal(err)
			}
			if err = m.seedTail(v); err != nil {
				b.Fatal(err)
			}
		} else {
			if err := m.apply(f.view); err != nil {
				b.Fatal(err)
			}
		}
		s := m.Snapshot()
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				benchmarkFrame = RenderFrame(s, UIState{Theme: Theme{Plain: true}, cache: &bodyCache{}}, 80, 24)
			}
		})
	}
}

func TestBackgroundWorkPrefixStableAcrossLoadedRangeAndLiveAppend(t *testing.T) {
	var entries []Entry
	entries = append(entries, Entry{Sequence: 1, Role: "peer", PeerID: "child", PeerKind: "report", Text: "report"})
	prefix := &host.HistoryWork{Entries: 301, Tools: 300, Reports: []session.ID{"child"}, Through: 301}
	for i := range 300 {
		id := session.TurnID(fmt.Sprintf("t-%d", i))
		prefix.Turns = append(prefix.Turns, id)
		entries = append(entries, Entry{Sequence: sessionstore.Sequence(i + 2), Role: "agent", Text: "background body", TurnID: id, Calls: []Receipt{{Name: "Read"}}})
	}
	s := Snapshot{ID: "s", Connected: true, Running: true, LazyHistory: true, HistoryBackground: true, HistoryWorkPrefix: prefix, TailBackground: true, Entries: append([]Entry(nil), entries[len(entries)-128:]...)}
	u := UIState{Theme: Theme{Plain: true}, Viewer: viewer.PanelSnapshot{ParentID: "s", Rows: []viewer.Row{{ID: "child", ParentID: "s", CreatedAt: time.Unix(1, 0)}}}}
	row := func(s Snapshot) string {
		for _, line := range strings.Split(chatText(s, u, 160), "\n") {
			if strings.Contains(line, "裏の作業") {
				return line
			}
		}
		t.Fatal("missing background summary")
		return ""
	}
	limited := row(s)
	if !strings.Contains(limited, "300ターン · ツール300回 · 報告 [A]") {
		t.Fatal("tail omitted prefix totals", limited)
	}
	all := s
	all.HistoryWorkPrefix = nil
	all.HistoryBackground = false
	all.Entries = entries
	if row(all) != limited {
		t.Fatal("background totals depend on loaded range", row(all), limited)
	}
	// A later continuation of an existing turn adds one tool, not one turn.
	s.Entries = append(s.Entries, Entry{Sequence: 302, Role: "agent", Text: "later", TurnID: "t-299", Calls: []Receipt{{Name: "Read"}}})
	if live := row(s); !strings.Contains(live, "300ターン · ツール301回") {
		t.Fatal("live append recounted prefix or turn", live)
	}
}

func TestLazyHistoryAttentionIndependentOfLoadedRangeAndClearedByReply(t *testing.T) {
	f := lazyFixture(3000)
	m := NewModel("s")
	v, err := f.InspectBefore(t.Context(), "s", 0, 128)
	if err != nil {
		t.Fatal(err)
	}
	v.HistoryState = &host.HistoryState{Attention: []string{"Question outside tail"}}
	if err = m.seedTail(v); err != nil {
		t.Fatal(err)
	}
	snapshot := m.Snapshot()
	snapshot.Entries = nil
	band := attentionBand(snapshot, UIState{}, makeLayout(120, 24))
	if len(band) != 1 || !strings.Contains(band[0].plain(), "Question outside tail") {
		t.Fatal("omitted question not shown")
	}
	snapshot.Attention[0] = "mutated"
	if m.Snapshot().Attention[0] != "Question outside tail" {
		t.Fatal("snapshot aliases attention")
	}
	v.History.Items = []host.HistoryItem{{Sequence: 3001, Kind: sessionstore.ItemInput, Data: inbox.Input{Kind: inbox.InputExternal, Payload: []byte(`"answer"`)}}}
	if err = m.apply(v); err != nil {
		t.Fatal(err)
	}
	if band := attentionBand(m.Snapshot(), UIState{}, makeLayout(120, 24)); len(band) != 0 {
		t.Fatal("answered question remains")
	}
}

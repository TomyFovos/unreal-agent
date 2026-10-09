package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func TestStatisticsFakeStreamUpdatesWithoutMovingComposerOrSubmitting(t *testing.T) {
	f := newFake()
	f.view.History.Items = []host.HistoryItem{{Sequence: 1, Kind: sessionstore.ItemHostRecord, Data: sessionstore.HostRecord{Kind: sessionstore.HostRuntimeApplied, Selection: &sessionstore.RuntimeSelection{Provider: "openai-codex", Model: "fixture-model", Effort: "high", Revision: 1}}}}
	f.view.History.NextAfter = 1
	keys := make(chan Key, 8)
	out := &screenObserver{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{Client: f, ID: "s", Keys: keys, Output: out, Theme: &Theme{Plain: true, Still: true}})
	}()
	defer func() {
		cancel()
		if err := <-done; err != nil && err != context.Canceled {
			t.Fatal(err)
		}
	}()
	waitFor(t, func() bool { return strings.Contains(out.text(), "Codex/fixture-model") })
	keys <- Key{Text: "unsent composer"}
	waitFor(t, func() bool { return strings.Contains(out.text(), "unsent composer") })
	for i, output := range []int64{3, 8} {
		stats := &llm.GenerationStatistics{Model: "fixture-model", Source: "client_clock", DurationMicros: 1_000_000, Tokens: llm.TokenCounts{Input: 12, Output: output, InputKnown: true, OutputKnown: true}}
		f.events <- host.Event{Generation: "g", Revision: uint64(i + 1), Kind: "progress", Progress: &host.Progress{TurnID: "turn", Epoch: 1, Attempt: 1, Mode: "streaming", Statistics: stats}}
		want := "TPS~" + strconvQuoteRate(output) + "(est)"
		waitFor(t, func() bool { return strings.Contains(out.text(), want) })
		if !strings.Contains(out.text(), "unsent composer") || out.cursorY() != 22 {
			t.Fatal("stream statistics moved/cleared composer", out.text())
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.inputs) != 0 {
		t.Fatal("statistics submitted control/chat input")
	}
}

func strconvQuoteRate(value int64) string { return fmt.Sprintf("%.1f", float64(value)) }

func (s *screenObserver) cursorY() int { s.mu.Lock(); defer s.mu.Unlock(); return s.y }

func statsFixture(provider string) Snapshot {
	s := idleSnapshot()
	s.Selection = &sessionstore.RuntimeSelection{Provider: provider, Model: "fixture-model", Effort: "high", Revision: 1}
	s.LastGeneration = &llm.GenerationStatistics{Model: "fixture-model", Source: "provider_api", DurationMicros: 2_000_000, Tokens: llm.TokenCounts{Input: 120, Output: 20, InputKnown: true, OutputKnown: true}}
	return s
}

func TestStatisticsFooterSourcesUnknownAndResponsiveComposer(t *testing.T) {
	for _, provider := range []string{"openai-codex", "claude-code"} {
		for _, view := range []ViewMode{ViewChat, ViewSplit, ViewOrchestration} {
			for _, size := range [][2]int{{1, 1}, {19, 5}, {44, 14}, {80, 24}, {120, 40}, {160, 40}} {
				s := statsFixture(provider)
				u := UIState{Theme: Theme{Plain: true}, Input: "input 日本語 👩🏽‍💻", Cursor: 3, View: view}
				f := RenderFrame(s, u, size[0], size[1])
				bounds(t, f, size[0], size[1])
				bar := f.Lines[len(f.Lines)-1]
				if size[0] >= 44 && (!strings.Contains(bar, "TPS 10.0(api)") || !strings.Contains(bar, "high")) {
					t.Fatal("rate/effort missing", bar)
				}
				if size[0] >= 80 && (!strings.Contains(bar, "fixture-model") || !strings.Contains(bar, "120") || !strings.Contains(bar, "20")) {
					t.Fatal("model/usage missing", bar)
				}
				if size[1] >= 14 && f.CursorY != size[1]-2 {
					t.Fatal("status row displaced input", f.CursorY)
				}
			}
		}
	}
	s := statsFixture("openai-codex")
	u := UIState{Theme: Theme{Plain: true}}
	s.Progress = &host.Progress{TurnID: "turn", Epoch: 1, Mode: "streaming"}
	if bar := statisticsBar(s, u, makeLayout(120, 24)).plain(); !strings.Contains(bar, "TPS —") || strings.Contains(bar, "10.0") {
		t.Fatal("new generation displayed stale previous rate", bar)
	}
	s.Progress = nil
	s.LastGeneration.Source = "client_clock"
	if text := statisticsBar(s, u, makeLayout(120, 24)).plain(); !strings.Contains(text, "TPS~10.0(est)") {
		t.Fatal(text)
	}
	s.LastGeneration = nil
	if text := statisticsBar(s, u, makeLayout(120, 24)).plain(); !strings.Contains(text, "TPS —") || !strings.Contains(text, "In:— Out:—") {
		t.Fatal("unknown fabricated", text)
	}
	u.Theme.ASCII = true
	if text := statisticsBar(s, u, makeLayout(120, 24)).plain(); strings.Contains(text, "—") || !strings.Contains(text, "TPS -") {
		t.Fatal(text)
	}
	s = statsFixture("claude-code")
	s.Selection.Model = "changed-model"
	if text := statisticsBar(s, u, makeLayout(120, 24)).plain(); strings.Contains(text, "10.0") || strings.Contains(text, "120") {
		t.Fatal("old runtime statistics labeled as new", text)
	}
}

func TestStatisticsStreamingCompletionReconnectAndCopyIsolation(t *testing.T) {
	m := NewModel("s")
	m.state = statsFixture("claude-code")
	m.state.Progress = &host.Progress{TurnID: "turn", Epoch: 1, Mode: "streaming", Statistics: &llm.GenerationStatistics{Model: "fixture-model", Source: "client_clock", DurationMicros: 1_000_000, Tokens: llm.TokenCounts{Input: 12, Output: 3, InputKnown: true, OutputKnown: true}}}
	u := UIState{Theme: Theme{Plain: true}}
	if bar := statisticsBar(m.Snapshot(), u, makeLayout(120, 24)).plain(); !strings.Contains(bar, "TPS~3.0(est)") || !strings.Contains(bar, "Out:3") {
		t.Fatal(bar)
	}
	response := llm.Response{Model: "fixture-model", Generation: &llm.GenerationStatistics{Model: "fixture-model", Source: "provider_api", DurationMicros: 2_000_000, Tokens: llm.TokenCounts{Input: 20, Output: 10, InputKnown: true, OutputKnown: true}}}
	v := host.View{Generation: "g", Running: true, History: host.HistoryPage{Items: []host.HistoryItem{{Sequence: 1, Kind: sessionstore.ItemModelResponse, Data: sessionstore.ModelResponse{TurnID: "turn", Response: response}}}}}
	if err := m.apply(v); err != nil {
		t.Fatal(err)
	}
	m.status("disconnected", false)
	bar := statisticsBar(m.Snapshot(), u, makeLayout(120, 24)).plain()
	if !strings.Contains(bar, "TPS 5.0(api)") || !strings.Contains(bar, "Out:10") {
		t.Fatal("completed rate lost on detach", bar)
	}
	if err := m.apply(v); err != nil {
		t.Fatal(err)
	}
	snapshot := m.Snapshot()
	snapshot.LastGeneration.Tokens.Output = 999
	if m.Snapshot().LastGeneration.Tokens.Output != 10 {
		t.Fatal("statistics alias exposed")
	}
	fresh := NewModel("s")
	if err := fresh.apply(v); err != nil || fresh.Snapshot().LastGeneration == nil || *fresh.Snapshot().LastGeneration != *response.Generation {
		t.Fatal("attach cannot reconstruct canonical statistics", err)
	}
}

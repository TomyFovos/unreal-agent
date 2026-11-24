package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/analysis"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
)

func TestCacheStatisticsLatestTotalSplitInspectAndExport(t *testing.T) {
	f := newFake()
	f.view.History.Items = nil
	for i, usage := range []llm.Usage{{InputTokens: 900, CachedInputTokens: 810}, {InputTokens: 100, CachedInputTokens: 3}} {
		turn := session.TurnID(string(rune('a' + i)))
		f.view.History.Items = append(f.view.History.Items,
			host.HistoryItem{Sequence: sessionstore.Sequence(2*i + 1), Kind: sessionstore.ItemTurn, Data: session.Turn{ID: turn, Type: session.TurnRegular}},
			host.HistoryItem{Sequence: sessionstore.Sequence(2*i + 2), Kind: sessionstore.ItemModelResponse, Data: sessionstore.ModelResponse{TurnID: turn, Response: llm.Response{Usage: usage, Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "Public answer."}}}}}},
		)
	}
	f.view.History.Items = append(f.view.History.Items, host.HistoryItem{Sequence: 5, Kind: sessionstore.ItemHostRecord, Data: sessionstore.HostRecord{Version: 1, Kind: sessionstore.HostContext, Context: &sessionstore.ContextRecord{Kind: "prune", PrunedTokens: 15000, Projections: []sessionstore.ContextProjection{{UnitID: "one"}, {UnitID: "two"}}}}})
	f.view.History.NextAfter = 5
	m := NewModel("s")
	if err := m.apply(f.view); err != nil {
		t.Fatal(err)
	}
	s := m.Snapshot()
	if viewer.CacheLabel(s.CacheLatest) != "cache 3.00%" || viewer.CacheLabel(s.CacheTotal) != "cache 81.30%" {
		t.Fatal("incorrect latest/cumulative ratios")
	}
	bar := statisticsBar(s, UIState{}, makeLayout(200, 40)).plain()
	if !strings.Contains(bar, "キャッシュ ▕████████░░▏81.30%") {
		t.Fatal("Split statistics missing the cumulative ratio", bar)
	}
	report := m.Analysis(time.Now(), nil)
	overview := analysisText(report, "Overview", 100, false)
	if _, cache, ok := strings.Cut(overview, "Cache read Session"); !ok || !strings.Contains(strings.SplitN(cache, "\n", 2)[0], "81.3%") {
		t.Fatal("analysis overview and footer disagree", overview)
	}
	inspect := strings.Join(analysis.Lines(report, "Usage", true), "\n")
	exported, err := analysis.Encode(report, "markdown")
	if err != nil {
		t.Fatal(err)
	}
	conversation, err := ConversationText(t.Context(), f, "s", 5)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{inspect, string(exported), conversation} {
		if !strings.Contains(text, "Latest cache 3.00%") || !strings.Contains(text, "Session cache 81.30%") {
			t.Fatal("inspect/export cache ratios missing", text)
		}
		if !strings.Contains(text, "Average input tokens / call  500.0 (2 reported calls)") || !strings.Contains(text, "Pruned tool results  2") || !strings.Contains(text, "estimated tokens removed 15000") || !strings.Contains(text, "epochs 1") {
			t.Fatal("input/prune metrics missing", text)
		}
	}
	f.view.History.Items = append(f.view.History.Items, host.HistoryItem{Sequence: 6, Kind: sessionstore.ItemHostRecord, Data: sessionstore.HostRecord{
		Version: 1, Kind: sessionstore.HostContext, Context: &sessionstore.ContextRecord{
			Conversation: &llm.ConversationRecord{Phase: "compact", Usage: &llm.Usage{InputTokens: 100, CachedInputTokens: 80}},
		},
	}})
	f.view.History.NextAfter = 6
	if err := m.apply(f.view); err != nil {
		t.Fatal(err)
	}
	footer := viewer.CacheLabel(m.Snapshot().CacheTotal)
	if want := viewer.CacheLabel(m.Analysis(time.Now(), nil).Usage); footer != "cache 81.18%" || footer != want {
		t.Fatalf("historical compaction usage differs: footer %s, analysis %s", footer, want)
	}
}

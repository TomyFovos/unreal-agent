package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
)

// A child's card shows its own latest context size, output speed and cache
// hit as bare numbers; unknown values are left out.
func TestCardStatsShowChildNumbersOnly(t *testing.T) {
	r := viewer.Row{LastUsage: viewer.Usage{Known: true, Input: 120_000, CachedInput: 110_400}, OutputTPS: 45.3}
	if got := cardStats(r); got != "120k · 45 t/s · 92.0%" {
		t.Fatalf("stats = %q", got)
	}
	if got := cardStats(viewer.Row{OutputTPS: 12}); got != "12 t/s" {
		t.Fatalf("speed only = %q", got)
	}
	if got := cardStats(viewer.Row{}); got != "" {
		t.Fatalf("nothing known = %q", got)
	}
	// The cache hit rate keeps a decimal; the context size does not.
	r = viewer.Row{LastUsage: viewer.Usage{Known: true, Input: 83_417, CachedInput: 82_816}, OutputTPS: 66.5}
	if got := cardStats(r); got != "83k · 66 t/s · 99.3%" {
		t.Fatalf("decimal placement = %q", got)
	}
}

func TestCardRowsPlaceChildNumbersOnTheSecondLine(t *testing.T) {
	child := workCard{id: "c", mark: "AB", title: "review", model: "gpt-6-astra · max", stats: "120k · 45 t/s · 92.0%", observed: true}
	rows := workCardRows(child, 100, Theme{Plain: true}, false)
	if len(rows) != 2 || !strings.Contains(rows[1].plain(), "120k · 45 t/s · 92.0%") {
		t.Fatalf("child rows = %q", []string{rows[0].plain(), rows[1].plain()})
	}
	narrow := workCardRows(child, 30, Theme{Plain: true}, false)
	if strings.Contains(narrow[1].plain(), "t/s") {
		t.Fatal("a narrow card keeps room for the current work", narrow[1].plain())
	}
	main := workCardRows(workCard{id: "m", title: "◆ メイン", main: true, stats: "1k", observed: true}, 100, Theme{Plain: true}, false)
	if strings.Contains(main[1].plain(), "1k") {
		t.Fatal("the main agent's numbers belong to the footer")
	}
}

func TestCardStatsFromClaudeResponse(t *testing.T) {
	usage := llm.Usage{InputTokens: 1000, CachedInputTokens: 700, CacheWriteInputTokens: 200, OutputTokens: 100}
	generation := &llm.GenerationStatistics{Tokens: llm.ReportedTokens(usage), DurationMicros: 2_000_000, Source: "client_clock"}
	v := host.View{Session: sessionstore.Snapshot{Session: session.Session{ID: "claude"}}, History: host.HistoryPage{NextAfter: 2, Items: []host.HistoryItem{
		{Sequence: 1, Kind: sessionstore.ItemHostRecord, Data: sessionstore.HostRecord{Version: 1, Kind: sessionstore.HostActivity, Activity: &llm.ActivityRecord{Kind: "claude_stream", Provider: "claude-code"}}},
		{Sequence: 2, Kind: sessionstore.ItemModelResponse, Data: sessionstore.ModelResponse{Response: llm.Response{Usage: usage, Generation: generation}}},
	}}}
	m := viewer.New(viewer.Options{})
	if err := m.Replace("claude", v); err != nil {
		t.Fatal(err)
	}
	if got := cardStats(m.Rows(time.Now())[0]); got != "1k · 50 t/s · 70.0%" {
		t.Fatalf("Claude card statistics = %q", got)
	}
}

package tui

import (
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/viewer"
)

// A child's card shows its own latest context size, output speed and cache
// hit as bare numbers; unknown values are left out.
func TestCardStatsShowChildNumbersOnly(t *testing.T) {
	r := viewer.Row{LastUsage: viewer.Usage{Known: true, Input: 120_000, CachedInput: 110_400}, OutputTPS: 45.3}
	if got := cardStats(r); got != "120k · 45 tok/s · 92%" {
		t.Fatalf("stats = %q", got)
	}
	if got := cardStats(viewer.Row{OutputTPS: 12}); got != "12 tok/s" {
		t.Fatalf("speed only = %q", got)
	}
	if got := cardStats(viewer.Row{}); got != "" {
		t.Fatalf("nothing known = %q", got)
	}
}

func TestCardRowsPlaceChildNumbersOnTheSecondLine(t *testing.T) {
	child := workCard{id: "c", mark: "AB", title: "review", model: "gpt-6-astra · max", stats: "120k · 45 tok/s · 92%", observed: true}
	rows := workCardRows(child, 100, Theme{Plain: true}, false)
	if len(rows) != 2 || !strings.Contains(rows[1].plain(), "120k · 45 tok/s · 92%") {
		t.Fatalf("child rows = %q", []string{rows[0].plain(), rows[1].plain()})
	}
	narrow := workCardRows(child, 30, Theme{Plain: true}, false)
	if strings.Contains(narrow[1].plain(), "tok/s") {
		t.Fatal("a narrow card keeps room for the current work", narrow[1].plain())
	}
	main := workCardRows(workCard{id: "m", title: "◆ メイン", main: true, stats: "1k", observed: true}, 100, Theme{Plain: true}, false)
	if strings.Contains(main[1].plain(), "1k") {
		t.Fatal("the main agent's numbers belong to the footer")
	}
}

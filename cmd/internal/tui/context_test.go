package tui

import (
	"github.com/rivo/uniseg"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/analysis"
	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/host"
)

func TestContextAnalysisDerivedMetricsUnknownUsageAndResponsiveSheets(t *testing.T) {
	m := NewModel("context-session")
	d := contextengine.Diagnostics{Version: 1, Provider: "claude-code", Model: "synthetic-model", RuntimeRevision: 4, ThroughSequence: 900, CheckpointBoundary: 768, CheckpointVersion: 1, CanonicalUnits: 600, Recent: 12, Retrieved: 3, Referenced: 2, Omitted: 583, EstimatedInputTokens: 4500, Budget: contextengine.Budget{Window: 32768, Input: 5000, WindowSource: "conservative-fallback", ResponseReserve: 4096, ProtocolReserve: 1024}, Utilization: 90, RetrievalMicros: 42, Measurement: "estimated", Cache: "rebuilt-canonical"}
	if err := m.apply(host.View{Generation: "g", Revision: 1, Running: true, Context: &d}); err != nil {
		t.Fatal(err)
	}
	r := m.Analysis(time.Now(), nil)
	if r.ContextPackage == nil || r.ContextPackage.EstimatedInputTokens != 4500 || r.Usage.Known {
		t.Fatal("estimated context became measured provider usage")
	}
	lines := strings.Join(analysis.Lines(r, "Context", true), "\n")
	for _, text := range []string{"estimated (not actual usage)", "Canonical context units  600", "Retrieved                3", "through sequence 768", "4500 / 5000", "input usage unknown"} {
		if !strings.Contains(lines, text) {
			t.Fatal("context metric missing", text)
		}
	}
	for _, format := range []string{"json", "markdown"} {
		data, err := analysis.Encode(r, format)
		if err != nil || !strings.Contains(strings.ToLower(string(data)), "estimated") {
			t.Fatal("context export missing", format, err)
		}
	}
	for _, size := range [][2]int{{80, 24}, {60, 20}, {44, 14}, {130, 30}} {
		for _, theme := range []Theme{{}, {NoColor: true}, {Plain: true}, {ASCII: true}} {
			// Existing renderer tests also cover the shared sheet viewport and
			// bottom composer; these lines use exactly that sheet path.
			u := UIState{Theme: theme, Sheet: &Sheet{Title: "Analysis · Context", Analysis: "Context", Lines: analysis.Lines(r, "Context", theme.ASCII)}}
			frame := RenderFrame(m.Snapshot(), u, size[0], size[1])
			if len(frame.Lines) != size[1] || frame.CursorY != size[1]-1 || !strings.Contains(frame.Lines[len(frame.Lines)-1], "Ask") {
				t.Fatal("context sheet displaced bottom composer")
			}
			for _, line := range frame.Lines {
				if uniseg.StringWidth(sgrRE.ReplaceAllString(line, "")) >= size[0] {
					t.Fatal("context sheet wrote rightmost column")
				}
				if theme.Plain && strings.Contains(line, "\x1b") {
					t.Fatal("plain sheet emitted ANSI")
				}
			}
		}
	}
	copy := m.Snapshot()
	copy.ContextPackage.EstimatedInputTokens = 1
	if m.Analysis(time.Now(), nil).ContextPackage.EstimatedInputTokens != 4500 {
		t.Fatal("snapshot mutated derived report")
	}
	if err := m.apply(host.View{Generation: "next", Revision: 1, Running: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(analysis.Lines(m.Analysis(time.Now(), nil), "Context", false), "\n"), "Context package unknown") {
		t.Fatal("stale context report survived a new Host without diagnostics")
	}
}

package tui

import (
	"fmt"
	"github.com/rivo/uniseg"
	"github.com/unreallabsai/unreal-agent/harness/analysis"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
	"strings"
	"testing"
)

func TestModelPickerEffortsScrollingFallbackAndBottom(t *testing.T) {
	current := &sessionstore.RuntimeSelection{Version: 1, Model: "m0", Name: "current", Provider: "openai-codex", Effort: llm.ReasoningEffortMedium}
	c := modelcatalog.Catalog{Available: true}
	for i := range 12 {
		c.Models = append(c.Models, modelcatalog.Model{ID: fmt.Sprintf("m%d", i), Name: fmt.Sprintf("Model %d", i), Efforts: []llm.ReasoningEffort{llm.ReasoningEffortHigh}, DefaultEffort: llm.ReasoningEffortHigh})
	}
	p := modelPicker(c, current)
	for range 11 {
		p.move(1)
	}
	if p.Selection != 11 {
		t.Fatal("unreachable model")
	}
	effort := p.effort(current)
	if effort.Kind != "effort" || len(effort.Options) != 1 || effort.Options[0] != "high" || current.Model != "m0" {
		t.Fatal("model picker changed runtime before final confirmation")
	}
	for _, size := range [][2]int{{80, 24}, {60, 20}, {44, 14}, {130, 30}} {
		for _, theme := range []Theme{{}, {Plain: true}, {NoColor: true}, {ASCII: true}} {
			u := UIState{Picker: p, Theme: theme}
			f := RenderFrame(Snapshot{ID: "test", Connected: true, Running: true, Selection: current}, u, size[0], size[1])
			text := strings.Join(f.Lines, "\n")
			if !strings.Contains(text, "Model 11") || f.CursorY != size[1]-1 || !strings.Contains(f.Lines[len(f.Lines)-1], "Ask") {
				t.Fatal(size, text)
			}
			for _, line := range f.Lines {
				if uniseg.StringWidth(sgrRE.ReplaceAllString(line, "")) >= size[0] {
					t.Fatal("rightmost cell written")
				}
			}
		}
	}
	fallback := modelPicker(modelcatalog.Catalog{}, current)
	if len(fallback.Options) != 1 || !strings.Contains(fallback.Hint, "catalog unavailable") || fallback.effort(current).Options[0] != "medium" {
		t.Fatal(fallback)
	}
	var d Decoder
	if len(d.Feed([]byte("\x1b"))) != 0 || d.FlushEscape()[0].Name != "escape" {
		t.Fatal("escape timeout")
	}
	if len(d.Feed([]byte("\x1b["))) != 0 || len(d.FlushEscape()) != 0 || d.Feed([]byte("A"))[0].Name != "up" {
		t.Fatal("split navigation")
	}
}

func TestAnalysisViewsResponsiveSanitizedScrolledAndCurrentHeader(t *testing.T) {
	choice := &sessionstore.RuntimeSelection{Version: 1, Provider: "openai-codex", Model: "m", Name: "GPT-6.1 Sol", Effort: llm.ReasoningEffortMedium}
	r := analysis.Report{Version: 1, Session: "test", Selection: &analysis.Selection{Model: "m", Name: "GPT-6.1 Sol", Effort: "medium"}, Usage: viewer.Usage{Known: true, Partial: true, Responses: 3, Input: 12}, Partial: true, Resync: true}
	for i := range 80 {
		r.Turns = append(r.Turns, analysis.Turn{Number: i + 1, Selection: r.Selection, Usage: viewer.Usage{Known: true, Input: int64(i + 1)}})
		r.Context = append(r.Context, r.Turns[i])
		r.ByTool = append(r.ByTool, analysis.Tool{Name: fmt.Sprintf("tool-%d", i), Counts: analysis.Counts{Calls: 1}})
	}
	r.ByTool[len(r.ByTool)-1].Name = strings.Repeat("資料e\u0301👩🏽‍💻", 12) + " FINAL_TOOL\x1b[2J\x1b]52;bad\a"
	s := Snapshot{ID: "test", Connected: true, Running: true, Selection: choice}
	for _, size := range [][2]int{{80, 24}, {60, 20}, {44, 14}, {130, 30}} {
		for _, theme := range []Theme{{}, {NoColor: true}, {Plain: true}, {ASCII: true}} {
			for _, view := range analysis.Views[:len(analysis.Views)-1] {
				u := UIState{Theme: theme, Sheet: &Sheet{Title: "Analysis · " + view, Analysis: view, Lines: analysis.Lines(r, view, theme.ASCII)}}
				f := RenderFrame(s, u, size[0], size[1])
				if len(f.Lines) != size[1] || f.CursorY != size[1]-1 || !strings.Contains(f.Lines[len(f.Lines)-1], "Ask") {
					t.Fatal("analysis displaced bottom composer", size, view)
				}
				for _, line := range f.Lines {
					if uniseg.StringWidth(sgrRE.ReplaceAllString(line, "")) >= size[0] || strings.Contains(line, "\x1b[2J") || strings.Contains(line, "\x1b]52") {
						t.Fatal("unsafe analysis rendering", size, view)
					}
					if theme.Plain && strings.Contains(line, "\x1b") {
						t.Fatal("plain analysis has ANSI")
					}
					if theme.NoColor {
						for _, seq := range sgrRE.FindAllString(line, -1) {
							if strings.Contains(seq, "3") || strings.Contains(seq, "4") {
								t.Fatal("NO_COLOR analysis has color", seq)
							}
						}
					}
				}
				if view == "Tools" {
					u.Sheet.Offset = 1_000_000
					end := RenderFrame(s, u, size[0], size[1])
					if end.SheetOffset == 0 || !strings.Contains(strings.Join(end.Lines, "\n"), "FINAL_TOOL") {
						t.Fatal("wrapped statistics unreachable", size, strings.Join(end.Lines, "\n"))
					}
				}
			}
		}
	}
	header := RenderFrame(s, UIState{Theme: Theme{Plain: true}}, 80, 24).Lines[0]
	if !strings.Contains(header, "GPT-6.1 Sol · medium") {
		t.Fatal("current selection missing from standard header", header)
	}
	if header := RenderFrame(s, UIState{Theme: Theme{Plain: true}}, 44, 14).Lines[0]; strings.Contains(header, "GPT-6.1 Sol") {
		t.Fatal("model did not yield space on tiny terminal", header)
	}
}

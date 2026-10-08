package tui

import (
	"github.com/rivo/uniseg"
	"github.com/unreallabsai/unreal-agent/harness/analysis"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
	"strings"
	"testing"
	"time"
)

func TestClaudeModelPickerUsesProviderCatalogAndExistingEffortNavigation(t *testing.T) {
	current := &sessionstore.RuntimeSelection{Version: 1, Provider: "claude-code", Model: "configured-a", Name: "A", Effort: "medium"}
	c, e := modelcatalog.Configured([]modelcatalog.Model{{ID: "configured-a", Name: "A", Efforts: []llm.ReasoningEffort{"low", "medium"}}, {ID: "configured-b", Name: "B", Efforts: []llm.ReasoningEffort{"high"}}})
	if e != nil {
		t.Fatal(e)
	}
	p := modelPicker(c, current)
	if p.Kind != "model" || !strings.Contains(p.Hint, "text-only") {
		t.Fatal("Claude capability not disclosed")
	}
	p.move(1)
	effort := p.effort(current)
	if len(effort.Options) != 1 || effort.Options[0] != "high" {
		t.Fatal("invented generic effort list")
	}
	if current.Model != "configured-a" || current.Effort != "medium" {
		t.Fatal("navigation committed runtime")
	}
	unavailable := modelPicker(modelcatalog.Catalog{}, current)
	if len(unavailable.Options) != 1 || !strings.Contains(unavailable.Hint, "catalog unavailable") {
		t.Fatal("guessed fallback catalog")
	}
	u := viewer.Usage{Known: true, Partial: true, Output: 3, Unknown: []llm.UsageField{llm.UsageInput}}
	if s := usage(u); !strings.Contains(s, "in unknown") || strings.Contains(s, "in 0") {
		t.Fatal("TUI treated unreported input as zero", s)
	}
}

func TestClaudeTrustedPolicyAnalysisKeepsLiveDockResponsive(t *testing.T) {
	m := NewModel("team")
	v := host.View{Running: true, Generation: "g", History: host.HistoryPage{NextAfter: 1, Items: []host.HistoryItem{{Sequence: 1, Kind: sessionstore.ItemHostRecord, Data: sessionstore.HostRecord{Kind: "configuration", Configuration: []byte(`{"Provider":{"provider":"claude-code","model":{"id":"configured-model"}},"ReasoningEffort":"medium","ClaudeCode":{"managedPolicyMode":"trust"}}`)}}}}}
	if err := m.apply(v); err != nil {
		t.Fatal(err)
	}
	r := m.Analysis(time.Now(), nil)
	if r.ManagedPolicyMode != "trust" {
		t.Fatal("TUI analysis omitted policy boundary")
	}
	for _, size := range [][2]int{{44, 14}, {60, 20}, {80, 24}, {130, 30}} {
		for _, theme := range []Theme{{}, {NoColor: true}, {Plain: true}, {ASCII: true}} {
			u := UIState{Theme: theme, Sheet: &Sheet{Title: "Analysis · Overview", Analysis: "Overview", Lines: analysis.Lines(r, "Overview", theme.ASCII)}}
			f := RenderFrame(m.Snapshot(), u, size[0], size[1])
			if len(f.Lines) != size[1] || f.CursorY != size[1]-1 || !strings.Contains(f.Lines[len(f.Lines)-1], "Ask") {
				t.Fatal("trust metadata displaced bottom composer", size)
			}
			for _, line := range f.Lines {
				if uniseg.StringWidth(sgrRE.ReplaceAllString(line, "")) >= size[0] || theme.Plain && strings.Contains(line, "\x1b") {
					t.Fatal("trust overview broke bounded/plain rendering", size)
				}
			}
			u.Sheet.Offset = 1_000_000
			if end := RenderFrame(m.Snapshot(), u, size[0], size[1]); end.CursorY != size[1]-1 {
				t.Fatal("scrolling overview moved composer")
			}
		}
	}
}

func TestClaudeClosedInitReasonVisibleInBoundedFailure(t *testing.T) {
	m := NewModel("team")
	failure := `call model for turn "00000000-0000-0000-0000-000000000000": init_plugins_loaded(count=1): claude-code is text-only: tools unsupported (Claude stream initialization violated isolation)`
	if err := m.apply(host.View{Generation: "g", Failure: failure}); err != nil {
		t.Fatal(err)
	}
	for _, size := range [][2]int{{44, 14}, {60, 20}, {80, 24}, {130, 30}} {
		for _, theme := range []Theme{{}, {NoColor: true}, {Plain: true}, {ASCII: true}} {
			f := RenderFrame(m.Snapshot(), UIState{Theme: theme}, size[0], size[1])
			text := sgrRE.ReplaceAllString(strings.Join(f.Lines, ""), "")
			if !strings.Contains(text, "init_plugins_loaded") || len(f.Lines) != size[1] || f.CursorY != size[1]-1 {
				t.Fatal("bounded failure hid the closed reason or moved composer", size)
			}
		}
	}
}

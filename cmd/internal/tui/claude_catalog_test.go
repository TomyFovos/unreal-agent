package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rivo/uniseg"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func TestClaudeDiscoveredPickerAliasRestrictionsEffortsAndResponsiveScroll(t *testing.T) {
	current := &sessionstore.RuntimeSelection{Version: 1, Provider: "claude-code", Model: "wire-a", Name: "A", Effort: "medium"}
	catalog := modelcatalog.Catalog{Available: true, Authoritative: true, Source: "Claude Code control catalog"}
	for i := range 12 {
		catalog.Models = append(catalog.Models, modelcatalog.Model{ID: fmt.Sprintf("alias-%d", i), Name: fmt.Sprintf("Discovered %d", i), Efforts: []llm.ReasoningEffort{"high", "xhigh"}})
	}
	catalog.Models[0].ResolvedModel = "wire-a"
	catalog.Models[0].Efforts = []llm.ReasoningEffort{"low", "medium"}
	p := modelPicker(catalog, current)
	if len(p.Options) != 12 || p.Selection != 0 || !strings.Contains(p.Hint, "discovered catalog") || p.effort(current).Selection != 1 {
		t.Fatal("wire ID did not match current alias or source metadata")
	}
	for range 11 {
		p.move(1)
	}
	if p.Selection != 11 || p.effort(current).Options[1] != "xhigh" {
		t.Fatal("model/effort navigation changed")
	}
	for _, size := range [][2]int{{44, 14}, {60, 20}, {80, 24}, {130, 30}} {
		for _, theme := range []Theme{{}, {NoColor: true}, {Plain: true}, {ASCII: true}} {
			frame := RenderFrame(Snapshot{ID: "team", Connected: true, Running: true, Selection: current}, UIState{Picker: p, Theme: theme}, size[0], size[1])
			if !strings.Contains(strings.Join(frame.Lines, "\n"), "Discovered 11") || frame.CursorY != size[1]-1 {
				t.Fatal("discovered model scrolled outside viewport or displaced composer", size)
			}
			for _, line := range frame.Lines {
				if uniseg.StringWidth(sgrRE.ReplaceAllString(line, "")) >= size[0] {
					t.Fatal("picker wrote rightmost column")
				}
			}
		}
	}
	current.Model = "blocked-model"
	p = modelPicker(catalog, current)
	if len(p.Options) != 12 || strings.Contains(strings.Join(p.Options, ""), "blocked-model") {
		t.Fatal("current model bypassed organization restrictions")
	}
	current.Model = "wire-a"
	p = modelPicker(modelcatalog.Catalog{Problem: "catalog unavailable"}, current)
	if len(p.Options) != 1 || p.Catalog.Models[0].ID != "wire-a" || p.effort(current).Options[0] != "medium" {
		t.Fatal("unavailable discovery invented options")
	}
	falseValue := false
	p = modelPicker(modelcatalog.Catalog{Available: true, Authoritative: true, Models: []modelcatalog.Model{{ID: "no-effort", Name: "No effort", SupportsEffort: &falseValue}}}, current)
	effort := p.effort(current)
	if len(effort.Options) != 1 || effort.selectedEffort() != "" || !strings.Contains(effort.Options[0], "No effort parameter") {
		t.Fatal("unsupported effort was invented")
	}
	if current.Model != "wire-a" || current.Effort != "medium" {
		t.Fatal("navigation mutated canonical selection")
	}
}

func TestClaudeModelRefreshAndNoEffortConfirmationRemainLocalUntilCommit(t *testing.T) {
	f := newFake()
	f.view.History.Items = []host.HistoryItem{{Sequence: 1, Kind: sessionstore.ItemHostRecord, Data: sessionstore.HostRecord{Version: 1, Kind: "configuration", Configuration: []byte(`{"Provider":{"provider":"claude-code","model":{"id":"old-model"}},"ReasoningEffort":"medium"}`)}}}
	falseValue := false
	catalog := modelcatalog.Catalog{Available: true, Authoritative: true, Models: []modelcatalog.Model{{ID: "no-effort", Name: "No effort model", SupportsEffort: &falseValue}}}
	keys := make(chan Key, 32)
	out := &screenObserver{}
	done := make(chan error, 1)
	selection := make(chan sessionstore.RuntimeSelection, 1)
	ctx, cancel := context.WithCancel(context.Background())
	refresh := make(chan struct{}, 1)
	go func() {
		done <- Run(ctx, Config{Client: f, ID: "s", Keys: keys, Output: out, Theme: &Theme{Plain: true}, Size: func() (int, int) { return 80, 24 }, RefreshModelCatalog: func(context.Context) (modelcatalog.Catalog, error) { refresh <- struct{}{}; return catalog, nil }, SelectModel: func(_ context.Context, _ string, _ uint64, s sessionstore.RuntimeSelection) error {
			selection <- s
			return nil
		}})
	}()
	t.Cleanup(func() {
		defer cancel()
		keys <- Key{Name: "detach"}
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("TUI did not detach")
		}
	})
	keys <- Key{Text: "/model refresh"}
	keys <- Key{Name: "enter"}
	select {
	case <-refresh:
	case <-time.After(3 * time.Second):
		t.Fatal("refresh used a cached-only callback")
	}
	waitFor(t, func() bool { return strings.Contains(out.text(), "No effort model") })
	keys <- Key{Name: "enter"}
	waitFor(t, func() bool { return strings.Contains(out.text(), "No effort parameter") })
	select {
	case <-selection:
		t.Fatal("model stage committed selection")
	default:
	}
	keys <- Key{Name: "enter"}
	select {
	case choice := <-selection:
		if choice.Model != "no-effort" || choice.Effort != "" || choice.Validate() != nil {
			t.Fatal("no-effort choice was invented/invalid")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("selection not confirmed")
	}
}

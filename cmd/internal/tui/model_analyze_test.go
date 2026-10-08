package tui

import (
	"context"
	"fmt"
	"github.com/unreallabsai/unreal-agent/harness/analysis"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunModelAndAnalyzePickersCancelScrollConfirmExportAndLiveUpdates(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {60, 20}, {44, 14}} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			f := newFake()
			f.view.Session.Session = session.Session{ID: "s", CreatedAt: time.Now()}
			f.view.History.Items = []host.HistoryItem{{Sequence: 1, Kind: sessionstore.ItemHostRecord, Data: sessionstore.HostRecord{Version: 1, Kind: "configuration", Configuration: []byte(`{"Provider":{"provider":"openai-codex","model":{"id":"m0"}},"ReasoningEffort":"medium","SystemPrompt":"prompt-sensitive"}`)}}}
			catalog := modelcatalog.Catalog{Available: true}
			for i := range 12 {
				catalog.Models = append(catalog.Models, modelcatalog.Model{ID: fmt.Sprintf("m%d", i), Name: fmt.Sprintf("Model %d", i), Efforts: []llm.ReasoningEffort{llm.ReasoningEffortHigh, llm.ReasoningEffortMax}, DefaultEffort: llm.ReasoningEffortHigh})
			}
			keys := make(chan Key, 128)
			out := &screenObserver{}
			done := make(chan error, 1)
			selected := make(chan sessionstore.RuntimeSelection, 2)
			var catalogCalls atomic.Int32
			exports := make(chan string, 2)
			ctx, cancel := context.WithCancel(context.Background())
			theme := Theme{Plain: true}
			go func() {
				done <- Run(ctx, Config{Client: f, ID: "s", Keys: keys, Output: out, Theme: &theme, Size: func() (int, int) { return size[0], size[1] }, ModelCatalog: func(context.Context) (modelcatalog.Catalog, error) { catalogCalls.Add(1); return catalog, nil }, SelectModel: func(_ context.Context, g string, revision uint64, s sessionstore.RuntimeSelection) error {
					if g != "g" || revision != 0 {
						t.Error("lost ownership/revision")
					}
					selected <- s
					return nil
				}, ExportAnalysis: func(_ context.Context, r analysis.Report, format string) (string, error) {
					if r.Selection == nil || r.Selection.Model != "m11" {
						t.Error("export lost canonical selection")
					}
					if _, err := analysis.Encode(r, format); err != nil {
						return "", err
					}
					exports <- format
					return "/private/state/export.json", nil
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
					cancel()
					t.Error("TUI did not detach")
				}
			})
			waitFor(t, func() bool { return strings.Contains(out.text(), "running") })
			command := func(text string) { t.Helper(); keys <- Key{Text: text}; keys <- Key{Name: "enter"} }
			pick := func(name string) {
				t.Helper()
				waitFor(t, func() bool {
					for _, line := range strings.Split(out.text(), "\n") {
						line = strings.TrimSpace(line)
						if strings.HasPrefix(line, "› ") && strings.Contains(line, name) {
							return true
						}
					}
					return false
				})
			}
			command("/model")
			pick("Model 0")
			keys <- Key{Name: "down"}
			pick("Model 1")
			keys <- Key{Name: "enter"}
			pick("high")
			select {
			case <-selected:
				t.Fatal("model committed before effort")
			default:
			}
			keys <- Key{Name: "escape"}
			pick("Model 0")
			keys <- Key{Name: "escape"}
			waitFor(t, func() bool {
				return !strings.Contains(out.text(), "Reasoning effort") && !strings.Contains(out.text(), "Model 0")
			})
			select {
			case <-selected:
				t.Fatal("cancel changed runtime")
			default:
			}
			command("/model")
			pick("Model 0")
			for i := 1; i < 12; i++ {
				keys <- Key{Name: "down"}
				pick(fmt.Sprintf("Model %d", i))
			}
			keys <- Key{Name: "enter"}
			pick("high")
			keys <- Key{Name: "tab"}
			pick("max")
			keys <- Key{Name: "enter"}
			var choice sessionstore.RuntimeSelection
			select {
			case choice = <-selected:
			case <-time.After(3 * time.Second):
				t.Fatal("selection not confirmed")
			}
			if choice.Model != "m11" || choice.Effort != llm.ReasoningEffortMax || choice.RequestID == "" {
				t.Fatal(choice)
			}
			waitFor(t, func() bool { return strings.Contains(out.text(), "Model changed") })
			choice.Revision = 1
			f.mu.Lock()
			f.view.Revision = 1
			f.view.History.Items = append(f.view.History.Items, host.HistoryItem{Sequence: 2, Kind: sessionstore.ItemHostRecord, Data: sessionstore.HostRecord{Version: 1, Kind: sessionstore.HostRuntimeSelection, Selection: &choice}}, host.HistoryItem{Sequence: 3, Kind: sessionstore.ItemHostRecord, Data: sessionstore.HostRecord{Version: 1, Kind: sessionstore.HostRuntimeApplied, Selection: &choice}})
			f.mu.Unlock()
			f.events <- host.Event{Generation: "g", Revision: 1, Kind: "item"}
			command("/analyze")
			pick("Overview")
			for _, view := range analysis.Views[1:] {
				keys <- Key{Name: "down"}
				pick(view)
			}
			keys <- Key{Name: "enter"}
			pick("JSON")
			keys <- Key{Name: "down"}
			pick("Markdown")
			keys <- Key{Name: "escape"}
			pick("Export")
			keys <- Key{Name: "enter"}
			pick("JSON")
			keys <- Key{Name: "enter"}
			select {
			case format := <-exports:
				if format != "JSON" {
					t.Fatal(format)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("export picker unreachable")
			}
			waitFor(t, func() bool { return strings.Contains(out.text(), "Analysis exported") })
			command("/analyze")
			pick("Overview")
			keys <- Key{Name: "down"}
			pick("Usage")
			keys <- Key{Name: "enter"}
			waitFor(t, func() bool { return strings.Contains(out.text(), "Analysis · Usage") })
			f.mu.Lock()
			f.view.Revision = 2
			f.view.History.Items = append(f.view.History.Items, host.HistoryItem{Sequence: 4, Kind: sessionstore.ItemTurn, Data: session.Turn{ID: "turn", RuntimeRevision: 1}}, host.HistoryItem{Sequence: 5, Kind: sessionstore.ItemModelResponse, Data: sessionstore.ModelResponse{TurnID: "turn", Response: llm.Response{Usage: llm.Usage{InputTokens: 7, OutputTokens: 2}}}})
			f.mu.Unlock()
			f.events <- host.Event{Generation: "g", Revision: 2, Kind: "item"}
			waitFor(t, func() bool { return strings.Contains(out.text(), "input         7") })
			keys <- Key{Name: "escape"}
			pick("Usage")
			keys <- Key{Name: "escape"}
			f.mu.Lock()
			if len(f.inputs) != 0 {
				t.Error("analysis/picker wrote canonical conversation")
			}
			f.mu.Unlock()
			if strings.Contains(out.rawText(), "prompt-sensitive") {
				t.Fatal("configuration prompt leaked")
			}
			// Paste does not open a command menu or run a command. Explicit
			// Enter dispatches registered commands, while masked entry stays private.
			keys <- Key{Text: "/model", Paste: true}
			keys <- Key{Name: "up"}
			keys <- Key{Name: "down"}
			keys <- Key{Name: "tab"}
			waitFor(t, func() bool {
				return strings.Contains(out.text(), "pasted:")
			})
			if catalogCalls.Load() != 2 {
				t.Fatal("paste opened a model picker before Enter")
			}
			keys <- Key{Name: "enter"}
			pick("Model 11")
			keys <- Key{Name: "escape"}
			waitFor(t, func() bool { return !strings.Contains(out.text(), "› Model 11") })
			command("/login openai private")
			waitFor(t, func() bool { return strings.Contains(out.text(), "private: API key") })
			privateText := "/analyze private-secret-sensitive"
			keys <- Key{Text: privateText}
			keys <- Key{Name: "up"}
			keys <- Key{Name: "down"}
			keys <- Key{Name: "enter"}
			waitFor(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.logins == 1 })
			f.mu.Lock()
			if len(f.inputs) != 0 || f.login != privateText {
				t.Error("new command names bypassed paste/private safety")
			}
			f.mu.Unlock()
			if catalogCalls.Load() != 3 || strings.Contains(out.rawText(), privateText) {
				t.Fatal("private entry opened a picker or exposed private input")
			}
		})
	}
}

func TestAnalysisMarksIncompleteCanonicalPagesUntilPagingCompletes(t *testing.T) {
	m := NewModel("s")
	v := host.View{Generation: "g", Running: true}
	v.Session.Session = session.Session{ID: "s", CreatedAt: time.Now()}
	v.History.More = true
	v.History.Items = []host.HistoryItem{{Sequence: 1, Kind: sessionstore.ItemHostRecord, Data: sessionstore.HostRecord{Version: 1, Kind: "configuration", Configuration: []byte(`{"Provider":{"provider":"openai-codex","model":{"id":"m"}},"ReasoningEffort":"medium"}`)}}}
	if err := m.apply(v); err != nil {
		t.Fatal(err)
	}
	partial := m.Analysis(time.Now(), nil)
	if !partial.Partial || !partial.Usage.Partial || partial.Errors.CrashesKnown {
		t.Fatal("incomplete page presented as complete statistics")
	}
	v.History.More = false
	v.History.Items = nil
	if err := m.apply(v); err != nil {
		t.Fatal(err)
	}
	complete := m.Analysis(time.Now(), nil)
	if complete.Partial || complete.Usage.Partial || !complete.Errors.CrashesKnown {
		t.Fatal("completed history kept an obsolete partial marker")
	}
}

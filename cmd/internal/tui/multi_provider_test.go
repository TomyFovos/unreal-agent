package tui

import (
	"context"
	"github.com/rivo/uniseg"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"strings"
	"testing"
	"time"
)

func TestMultiProviderPickerNamespacesCancelAndConfirm(t *testing.T) {
	for _, size := range [][2]int{{44, 14}, {60, 20}, {80, 24}, {130, 30}} {
		t.Run(string(rune(size[0])), func(t *testing.T) {
			f := newFake()
			f.view.Session.Session = session.Session{ID: "s"}
			f.view.History.Items = []host.HistoryItem{{Sequence: 1, Kind: sessionstore.ItemHostRecord, Data: sessionstore.HostRecord{Version: 1, Kind: "configuration", Configuration: []byte(`{"Provider":{"provider":"claude-code","model":{"id":"shared"}},"ReasoningEffort":"high"}`)}}}
			// Equal aliases in different namespaces must never borrow the other
			// provider's effort/current marker.
			catalogs := []modelcatalog.Provider{{ID: "claude-code", Name: "Claude Code", Availability: "available", Catalog: modelcatalog.Catalog{Available: true, Models: []modelcatalog.Model{{ID: "shared", Name: "Claude model", Efforts: []llm.ReasoningEffort{"high"}, DefaultEffort: "high"}}}}, {ID: "openai-codex", Name: "OpenAI Codex", Availability: "available", Tools: true, Catalog: modelcatalog.Catalog{Available: true, Models: []modelcatalog.Model{{ID: "shared", Name: "Codex model", Efforts: []llm.ReasoningEffort{"medium", "max"}, DefaultEffort: "medium"}}}}}
			keys := make(chan Key, 32)
			out := &screenObserver{}
			chosen := make(chan sessionstore.RuntimeSelection, 1)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() {
				done <- Run(ctx, Config{Client: f, ID: "s", Keys: keys, Output: out, Theme: &Theme{Plain: true}, Size: func() (int, int) { return size[0], size[1] }, ProviderCatalogs: func(context.Context) ([]modelcatalog.Provider, error) { return catalogs, nil }, ProviderCatalog: func(_ context.Context, id string, _ bool) (modelcatalog.Catalog, error) {
					for _, p := range catalogs {
						if p.ID == id {
							return p.Catalog, nil
						}
					}
					return modelcatalog.Catalog{}, nil
				}, SelectModel: func(_ context.Context, g string, revision uint64, s sessionstore.RuntimeSelection) error {
					if g != "g" || revision != 0 {
						t.Error("provider picker lost revision")
					}
					chosen <- s
					return nil
				}})
			}()
			t.Cleanup(func() {
				keys <- Key{Name: "detach"}
				select {
				case e := <-done:
					if e != nil {
						t.Error(e)
					}
				case <-time.After(3 * time.Second):
					t.Error("detach failed")
				}
				cancel()
			})
			waitFor(t, func() bool { return strings.Contains(out.text(), "running") })
			command := func() {
				keys <- Key{Text: "/model"}
				keys <- Key{Name: "enter"}
				waitFor(t, func() bool {
					return strings.Contains(out.text(), "Provider") && strings.Contains(out.text(), "OpenAI Codex")
				})
			}
			command()
			keys <- Key{Name: "down"}
			keys <- Key{Name: "enter"}
			waitFor(t, func() bool { return strings.Contains(out.text(), "Codex model") })
			keys <- Key{Name: "enter"}
			waitFor(t, func() bool {
				return strings.Contains(out.text(), "Reasoning effort") && strings.Contains(out.text(), "medium")
			})
			select {
			case <-chosen:
				t.Fatal("committed provider before final effort")
			default:
			}
			keys <- Key{Name: "escape"}
			waitFor(t, func() bool {
				return strings.Contains(out.text(), "Codex model") && !strings.Contains(out.text(), "Reasoning effort")
			})
			if strings.Contains(out.text(), "Claude model") {
				t.Fatal("cross-provider alias/current entry leaked into catalog")
			}
			keys <- Key{Name: "escape"}
			waitFor(t, func() bool { return strings.Contains(out.text(), "Provider") })
			keys <- Key{Name: "escape"}
			waitFor(t, func() bool { return !strings.Contains(out.text(), "Provider") })
			select {
			case <-chosen:
				t.Fatal("cancel changed canonical selection")
			default:
			}
			command()
			keys <- Key{Name: "tab"}
			keys <- Key{Name: "enter"}
			waitFor(t, func() bool { return strings.Contains(out.text(), "Codex model") })
			keys <- Key{Name: "enter"}
			waitFor(t, func() bool { return strings.Contains(out.text(), "medium") })
			keys <- Key{Name: "down"}
			keys <- Key{Name: "enter"}
			select {
			case s := <-chosen:
				if s.Provider != "openai-codex" || s.Model != "shared" || s.Effort != "max" {
					t.Fatal("provider namespace lost", s)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("no final selection")
			}
		})
	}
}

func TestMultiProviderPickerResponsiveBottomAndHeader(t *testing.T) {
	current := &sessionstore.RuntimeSelection{Version: 1, Provider: "claude-code", Model: "sonnet", Name: "Sonnet", Effort: "medium"}
	p := providerPicker([]modelcatalog.Provider{{ID: "claude-code", Name: "Claude Code", Availability: "available"}, {ID: "openai-codex", Name: "OpenAI Codex", Availability: "auth required"}}, current)
	for _, size := range [][2]int{{44, 14}, {60, 20}, {80, 24}, {130, 30}} {
		for _, theme := range []Theme{{}, {Plain: true}, {NoColor: true}, {ASCII: true}} {
			frame := RenderFrame(Snapshot{ID: "test", Connected: true, Running: true, Selection: current}, UIState{Theme: theme, Picker: p}, size[0], size[1])
			if len(frame.Lines) != size[1] || frame.CursorY != size[1]-1 {
				t.Fatal("provider picker displaced composer")
			}
			if size[0] >= 80 && !strings.Contains(strings.Join(frame.Lines, "\n"), "Claude Code") {
				t.Fatal("header lost provider")
			}
			for _, line := range frame.Lines {
				if uniseg.StringWidth(sgrRE.ReplaceAllString(line, "")) >= size[0] {
					t.Fatal("provider picker writes rightmost column")
				}
			}
		}
	}
}

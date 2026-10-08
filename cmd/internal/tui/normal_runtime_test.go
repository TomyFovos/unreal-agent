package tui

import (
	"context"
	"encoding/json/v2"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rivo/uniseg"
	"github.com/unreallabsai/unreal-agent/harness/analysis"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func TestNormalRuntimeCapabilityProjectionDoesNotRewriteDesiredConfiguration(t *testing.T) {
	m := NewModel("team")
	v := host.View{Running: true, Generation: "g", History: host.HistoryPage{NextAfter: 1, Items: []host.HistoryItem{{Sequence: 1, Kind: sessionstore.ItemHostRecord, Data: sessionstore.HostRecord{Kind: "configuration", Configuration: []byte(`{"Provider":{"provider":"claude-code","model":{"id":"alias"}},"ReasoningEffort":"high","ClaudeCode":{"managedPolicyMode":"trust","ToolBridge":{"Enabled":true}}}`)}}}}}
	if err := m.apply(v); err != nil {
		t.Fatal(err)
	}
	before := m.Snapshot()
	for _, status := range []string{"blocked_by_policy", "available", "unknown", "disabled"} {
		m.setCapabilities(map[string]modelcatalog.Capabilities{"claude-code": {Tools: status == "available", ToolBridge: status}})
		r := m.Analysis(time.Now(), nil)
		if r.ToolBridgeStatus != status || r.ToolBridgeEnabled != (status == "available") || r.ManagedPolicyMode != "trust" {
			t.Fatal("effective health conflated with desired configuration")
		}
		data, err := json.Marshal(r)
		if err != nil || !strings.Contains(string(data), `"ToolBridgeStatus":"`+status+`"`) {
			t.Fatal("analysis export omitted safe status", err)
		}
		for _, size := range [][2]int{{44, 14}, {60, 20}, {80, 24}, {130, 30}} {
			for _, theme := range []Theme{{}, {NoColor: true}, {Plain: true}, {ASCII: true}} {
				u := UIState{Theme: theme, Sheet: &Sheet{Title: "Analysis · Overview", Analysis: "Overview", Lines: analysis.Lines(r, "Overview", theme.ASCII)}}
				frame := RenderFrame(m.Snapshot(), u, size[0], size[1])
				if frame.CursorY != size[1]-1 {
					t.Fatal("health moved composer")
				}
				for _, line := range frame.Lines {
					if uniseg.StringWidth(sgrRE.ReplaceAllString(line, "")) >= size[0] {
						t.Fatal("health metadata broke right edge")
					}
				}
			}
		}
	}
	if !reflect.DeepEqual(before, m.Snapshot()) {
		t.Fatal("viewing health created conversation/operation")
	}
}

func TestNormalSingleProviderPickerSkipsProviderStageAndShowsBlockedBridge(t *testing.T) {
	f := newFake()
	f.view.History.Items = []host.HistoryItem{{Sequence: 1, Kind: sessionstore.ItemHostRecord, Data: sessionstore.HostRecord{Version: 1, Kind: "configuration", Configuration: []byte(`{"Provider":{"provider":"claude-code","model":{"id":"alias"}},"ReasoningEffort":"medium","ClaudeCode":{"managedPolicyMode":"trust","ToolBridge":{"Enabled":true}}}`)}}}
	current := &sessionstore.RuntimeSelection{Version: 1, Provider: "claude-code", Model: "alias", Effort: "medium", Binding: `{"ClaudeCode":{"ToolBridge":{"Enabled":true}}}`}
	catalog := modelcatalog.Catalog{Available: true, Authoritative: true, Models: []modelcatalog.Model{{ID: "alias", Name: "Claude model", Efforts: []llm.ReasoningEffort{"medium"}}}}
	p := modelPicker(catalog, current, false)
	p.capability(modelcatalog.Capabilities{ToolBridge: "blocked_by_policy"})
	if !strings.Contains(p.Hint, "text-only") || !strings.Contains(p.Hint, "blocked_by_policy") || strings.Contains(p.Hint, "Unreal tool bridge") {
		t.Fatal("canonical desired bridge overrode live blocked status")
	}
	keys := make(chan Key, 8)
	out := &screenObserver{}
	// The fixture must stay live through t.Cleanup's explicit detach.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{Client: f, ID: "s", Keys: keys, Output: out, Theme: &Theme{Plain: true}, Size: func() (int, int) { return 100, 28 }, ModelCatalog: func(context.Context) (modelcatalog.Catalog, error) { return catalog, nil }, ProviderCatalogs: func(context.Context) ([]modelcatalog.Provider, error) {
			return []modelcatalog.Provider{{ID: "claude-code", Name: "Claude Code", Availability: "available", ToolBridge: "blocked_by_policy", Catalog: catalog}}, nil
		}, RuntimeCapabilities: func(context.Context) (map[string]modelcatalog.Capabilities, error) {
			return map[string]modelcatalog.Capabilities{"claude-code": {ToolBridge: "blocked_by_policy"}}, nil
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
			t.Error("normal TUI did not detach")
		}
	})
	waitFor(t, func() bool { return strings.Contains(out.text(), "running") })
	keys <- Key{Text: "/model"}
	keys <- Key{Name: "enter"}
	waitFor(t, func() bool {
		return strings.Contains(out.text(), "Claude model") && strings.Contains(out.text(), "blocked_by_policy")
	})
	if strings.Contains(out.text(), "Provider\n") {
		t.Fatal("sole provider added a provider stage")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.inputs) != 0 {
		t.Fatal("picker sent model input")
	}
}

func TestNormalMultiProviderPickerKeepsObservedBridgeStatusThroughEffortCancel(t *testing.T) {
	f := newFake()
	f.view.History.Items = []host.HistoryItem{{Sequence: 1, Kind: sessionstore.ItemHostRecord, Data: sessionstore.HostRecord{Version: 1, Kind: "configuration", Configuration: []byte(`{"Provider":{"provider":"openai-codex","model":{"id":"gpt"}},"ReasoningEffort":"medium"}`)}}}
	catalog := modelcatalog.Catalog{Available: true, Authoritative: true, Models: []modelcatalog.Model{{ID: "alias", Name: "Claude model", Efforts: []llm.ReasoningEffort{"medium"}, DefaultEffort: "medium"}}}
	providers := []modelcatalog.Provider{{ID: "claude-code", Name: "Claude Code", Availability: "available", ToolBridge: "blocked_by_policy", Catalog: catalog}, {ID: "openai-codex", Name: "OpenAI Codex", Availability: "available", Tools: true}}
	keys := make(chan Key, 12)
	out := &screenObserver{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{Client: f, ID: "s", Keys: keys, Output: out, Theme: &Theme{Plain: true}, Size: func() (int, int) { return 140, 28 }, ProviderCatalogs: func(context.Context) ([]modelcatalog.Provider, error) {
			return providers, nil
		}, ProviderCatalog: func(_ context.Context, id string, _ bool) (modelcatalog.Catalog, error) {
			if id != "claude-code" {
				t.Error("wrong provider namespace")
			}
			return catalog, nil
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
			t.Error("normal TUI did not detach")
		}
	})
	waitFor(t, func() bool { return strings.Contains(out.text(), "running") })
	keys <- Key{Text: "/model"}
	keys <- Key{Name: "enter"}
	waitFor(t, func() bool {
		return strings.Contains(out.text(), "Provider") && strings.Contains(out.text(), "OpenAI Codex")
	})
	keys <- Key{Name: "up"}
	keys <- Key{Name: "enter"}
	waitFor(t, func() bool {
		return strings.Contains(out.text(), "Claude model") && strings.Contains(out.text(), "blocked_by_policy")
	})
	if !strings.Contains(out.text(), "text-only; discovered catalog") {
		t.Fatal("cross-provider picker lost effective text capability")
	}
	keys <- Key{Name: "enter"}
	waitFor(t, func() bool { return strings.Contains(out.text(), "Reasoning effort") })
	keys <- Key{Name: "escape"}
	waitFor(t, func() bool {
		return !strings.Contains(out.text(), "Reasoning effort") && strings.Contains(out.text(), "blocked_by_policy")
	})
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.inputs) != 0 {
		t.Fatal("viewing capability sent inference")
	}
}

package tui

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

type subagentModelRun struct {
	keys     chan Key
	out      *screenObserver
	mu       sync.Mutex
	saved    [][2]string
	selected int
}

func startSubagentModelRun(t *testing.T, providers []modelcatalog.Provider, pending bool) *subagentModelRun {
	t.Helper()
	f := newFake()
	f.view.Session.Session = session.Session{ID: "s"}
	r := &subagentModelRun{keys: make(chan Key, 32), out: &screenObserver{}}
	current := &sessionstore.RuntimeSelection{Version: 1, Provider: "openai-codex"}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{Client: f, ID: "s", Keys: r.keys, Output: r.out, Theme: &Theme{Plain: true}, Size: func() (int, int) { return 240, 30 },
			SubagentModels: func(context.Context) ([]modelcatalog.Provider, *sessionstore.RuntimeSelection, error) {
				r.mu.Lock()
				defer r.mu.Unlock()
				return providers, current, nil
			},
			ProviderCatalog: func(_ context.Context, id string, refresh bool) (modelcatalog.Catalog, error) {
				for _, p := range providers {
					if p.ID == id && !refresh {
						return p.Catalog, nil
					}
				}
				t.Error("catalog read", id, refresh)
				return modelcatalog.Catalog{}, nil
			},
			SelectSubagentModel: func(_ context.Context, provider, model string) (sessionstore.RuntimeSelection, bool, error) {
				r.mu.Lock()
				defer r.mu.Unlock()
				r.saved = append(r.saved, [2]string{provider, model})
				name := ""
				if model != "" {
					name = "DeepSeek V4.1 Flash"
				}
				current = &sessionstore.RuntimeSelection{Version: 1, Provider: provider, Model: model, Name: name}
				return *current, pending, nil
			},
			SelectModel: func(context.Context, string, uint64, sessionstore.RuntimeSelection) error {
				r.mu.Lock()
				defer r.mu.Unlock()
				r.selected++
				return nil
			}})
	}()
	t.Cleanup(func() {
		r.keys <- Key{Name: "detach"}
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
	waitFor(t, func() bool { return strings.Contains(r.out.text(), "入力待ち") })
	return r
}

func (r *subagentModelRun) open(t *testing.T) {
	t.Helper()
	r.keys <- Key{Text: "/model-sub"}
	r.keys <- Key{Name: "enter"}
	waitFor(t, func() bool {
		text := r.out.text()
		return strings.Contains(text, "Sub-agents · Provider") && strings.Contains(text, "Codex · available") && strings.Contains(text, "OpenCode Go")
	})
}

// shows reports text on screen, also when the notice wraps.
func (r *subagentModelRun) shows(text string) bool {
	flat := func(s string) string { return strings.Join(strings.Fields(s), "") }
	return strings.Contains(flat(r.out.text()), flat(text))
}

func (r *subagentModelRun) savedChoices() ([][2]string, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][2]string(nil), r.saved...), r.selected
}

var subagentProviders = []modelcatalog.Provider{
	{ID: "openai-codex", Name: "Codex", Availability: "available", Tools: true},
	{ID: "opencode-go", Name: "OpenCode Go", Availability: "available", Tools: true, Catalog: modelcatalog.Catalog{Available: true, Models: []modelcatalog.Model{{ID: "deepseek-v4.1-flash", Name: "DeepSeek V4.1 Flash", Efforts: []llm.ReasoningEffort{"low", "high", "max"}, DefaultEffort: "high"}}}},
}

// /model-sub lists Codex and OpenCode Go; OpenCode Go then lists its models.
// Codex needs no model. Neither changes the conversation's own model.
func TestModelSubChoosesTheSubagentsModel(t *testing.T) {
	r := startSubagentModelRun(t, subagentProviders, false)
	r.open(t)
	r.keys <- Key{Name: "down"}
	r.keys <- Key{Name: "enter"}
	waitFor(t, func() bool {
		return strings.Contains(r.out.text(), "OpenCode Go · Sub-agent model") && strings.Contains(r.out.text(), "DeepSeek V4.1 Flash")
	})
	// Escape goes back to the providers without saving.
	r.keys <- Key{Name: "escape"}
	waitFor(t, func() bool {
		return strings.Contains(r.out.text(), "Sub-agents · Provider") && !strings.Contains(r.out.text(), "Sub-agent model")
	})
	if saved, _ := r.savedChoices(); len(saved) != 0 {
		t.Fatal("escape saved a choice", saved)
	}
	r.keys <- Key{Name: "enter"}
	waitFor(t, func() bool { return strings.Contains(r.out.text(), "OpenCode Go · Sub-agent model") })
	r.keys <- Key{Name: "enter"}
	waitFor(t, func() bool {
		return r.shows("サブエージェントのモデルを変更しました: OpenCode Go · DeepSeek V4.1 Flash（次に起動する子から使います）")
	})
	r.open(t)
	r.keys <- Key{Name: "up"}
	r.keys <- Key{Name: "enter"}
	waitFor(t, func() bool {
		return r.shows("サブエージェントのモデルを変更しました: Codex（次に起動する子から使います）")
	})
	saved, selected := r.savedChoices()
	if len(saved) != 2 || saved[0] != [2]string{"opencode-go", "deepseek-v4.1-flash"} || saved[1] != [2]string{"openai-codex", ""} {
		t.Fatal("saved", saved)
	}
	if selected != 0 {
		t.Fatal("/model-sub changed the conversation's model")
	}
}

// OpenCode Go without its key says how to add it and saves nothing; a choice
// on a Host without sub-agents says when it applies.
func TestModelSubExplainsUnavailableAndPendingChoices(t *testing.T) {
	providers := []modelcatalog.Provider{subagentProviders[0], {ID: "opencode-go", Name: "OpenCode Go", Availability: "unavailable", Reason: modelcatalog.AuthRequired}}
	r := startSubagentModelRun(t, providers, true)
	r.open(t)
	r.keys <- Key{Name: "down"}
	r.keys <- Key{Name: "enter"}
	waitFor(t, func() bool { return strings.Contains(r.out.text(), "OpenCode Go · unavailable · /login") })
	if saved, _ := r.savedChoices(); len(saved) != 0 {
		t.Fatal("an unavailable provider was saved", saved)
	}
	r.keys <- Key{Name: "escape"}
	waitFor(t, func() bool { return !strings.Contains(r.out.text(), "Sub-agents · Provider") })
	r.open(t)
	r.keys <- Key{Name: "enter"}
	waitFor(t, func() bool {
		return r.shows("サブエージェントのモデルを保存しました: Codex（この Host にはサブエージェントがないため、Host が次に起動したときから使えます）")
	})
}

// The Claude choice names Claude Code's Agent model: its own next turn uses
// it, and running children keep theirs, whether or not the Host has
// sub-agents of its own.
func TestSubagentModelNoticeForClaudeCodeAgent(t *testing.T) {
	choice := sessionstore.RuntimeSelection{Version: 1, Provider: "claude", Model: "claude-configured-a", Name: "Discovered A"}
	want := "サブエージェントのモデルを変更しました: " + modelcatalog.ProviderName("claude") + " · Discovered A（Claude の次のターンから使えます。実行中の子はそのままのモデルで動きます）"
	if got := subagentModelNotice(modelcatalog.ProviderName("claude"), choice, false); got != want {
		t.Fatal(got)
	}
	if got := subagentModelNotice(modelcatalog.ProviderName("claude"), choice, true); got != want {
		t.Fatal("the pending wording on Claude", got)
	}
	if got := subagentModelNotice("Codex", sessionstore.RuntimeSelection{Provider: "openai-codex"}, true); !strings.Contains(got, "Host が次に起動したときから使えます") {
		t.Fatal("the children wording changed", got)
	}
}

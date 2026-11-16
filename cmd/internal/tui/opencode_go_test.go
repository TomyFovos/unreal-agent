package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

// /login explains how to save the OpenCode Go key in another shell; the TUI
// itself never asks for it (TestLoginShowsSubscriptionInstructionsWithoutRequestingKeys).
func TestLoginShowsOpenCodeGoKeyCommand(t *testing.T) {
	command := "pbpaste | /opt/unreal/unreal-agent-auth login --store ~/.local/state/unreal-agent/credentials --provider opencode-go --id primary --key-stdin"
	for _, given := range []string{command, ""} {
		f, keys, out := newFake(), make(chan Key, 8), &screenObserver{}
		done := make(chan error, 1)
		go func() {
			done <- Run(t.Context(), Config{Client: f, ID: "s", Keys: keys, Output: out, Theme: &Theme{Plain: true}, Size: func() (int, int) { return 260, 40 }, OpenCodeGoKeyCommand: given})
		}()
		waitFor(t, func() bool { return strings.Contains(out.text(), "入力待ち") })
		keys <- Key{Text: "/login"}
		keys <- Key{Name: "enter"}
		waitFor(t, func() bool { return strings.Contains(out.text(), "Extra Usage") })
		text := out.text()
		for _, want := range []string{"codex login", "claude を起動", "OpenCode Go", "キーは画面に出ません", "/model refresh", "docs/opencode-go.md"} {
			if !strings.Contains(text, want) {
				t.Fatal(want, text)
			}
		}
		if strings.Contains(text, command) != (given != "") || given == "" && strings.Contains(text, "pbpaste |") {
			t.Fatal("key command", given != "", text)
		}
		for _, gone := range []string{"PROVIDER ID", "API key", "store key", "private:", "pkill"} {
			if strings.Contains(text, gone) {
				t.Fatal(gone, text)
			}
		}
		keys <- Key{Name: "detach"}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		f.mu.Lock()
		if f.logins != 0 || len(f.inputs) != 0 {
			t.Fatal("login requested a key or wrote conversation")
		}
		f.mu.Unlock()
	}
}

// An OpenCode Go model whose capabilities name no Unreal effort is committed
// from /model without an effort step: the request carries no reasoning effort.
func TestModelPickerCommitsEffortUnknownModelWithoutEffortStep(t *testing.T) {
	f := newFake()
	f.view.History.Items = []host.HistoryItem{{Sequence: 1, Kind: sessionstore.ItemHostRecord, Data: sessionstore.HostRecord{Version: 1, Kind: "configuration", Configuration: []byte(`{"Provider":{"provider":"opencode-go","model":{"id":"kimi-k2.7-code"}},"ReasoningEffort":"high"}`)}}}
	catalog := modelcatalog.Catalog{Available: true, Authoritative: true, Models: []modelcatalog.Model{{ID: "kimi-k2.7-code", Name: "Kimi K2.7 Code", EffortUnknown: true}}}
	keys := make(chan Key, 32)
	out := &screenObserver{}
	done := make(chan error, 1)
	selection := make(chan sessionstore.RuntimeSelection, 1)
	go func() {
		done <- Run(t.Context(), Config{Client: f, ID: "s", Keys: keys, Output: out, Theme: &Theme{Plain: true}, Size: func() (int, int) { return 80, 24 }, ModelCatalog: func(context.Context) (modelcatalog.Catalog, error) {
			return catalog, nil
		}, SelectModel: func(_ context.Context, _ string, _ uint64, s sessionstore.RuntimeSelection) error {
			selection <- s
			return nil
		}})
	}()
	keys <- Key{Text: "/model"}
	keys <- Key{Name: "enter"}
	waitFor(t, func() bool { return strings.Contains(out.text(), "Kimi K2.7 Code") })
	keys <- Key{Name: "enter"}
	select {
	case choice := <-selection:
		if choice.Provider != "opencode-go" || choice.Model != "kimi-k2.7-code" || choice.Effort != "" {
			t.Fatal("effort-unknown model committed an invented effort", choice)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("model choice did not commit without an effort step")
	}
	if strings.Contains(out.text(), "No effort parameter") {
		t.Fatal("effort step opened for a model that names no Unreal effort")
	}
	keys <- Key{Name: "detach"}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

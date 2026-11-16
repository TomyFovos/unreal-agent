//go:build disable_opencode_go

package tui

import (
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/internal/buildfeatures"
	"github.com/unreallabsai/unreal-agent/internal/usertext"
)

func TestDisabledOpenCodeGoSavedSelectionNotice(t *testing.T) {
	choice := &sessionstore.RuntimeSelection{Version: 1, Provider: "opencode-go", Model: "saved-model", Effort: "high"}
	for _, s := range []Snapshot{{Selection: choice}, {PendingSelection: choice}} {
		frame := RenderFrame(s, UIState{Theme: Theme{Plain: true}}, 180, 32)
		if !strings.Contains(strings.Join(frame.Lines, "\n"), "この配布物では OpenCode Go は使えません") || !strings.Contains(strings.Join(frame.Lines, "\n"), "/model") {
			t.Fatal("missing immediate resume notice", strings.Join(frame.Lines, "\n"))
		}
	}
	main := modelPicker(modelcatalog.Catalog{}, choice)
	if len(main.Options) != 0 || main.Kind != "provider" {
		t.Fatal("disabled saved model remains in /model", main)
	}
	p := subagentProviderPicker([]modelcatalog.Provider{{ID: "openai-codex", Name: "Codex"}}, choice)
	if !strings.Contains(p.Hint, "この配布物では") || !strings.Contains(p.Hint, "/model-sub") {
		t.Fatal("missing saved child choice guidance", p.Hint)
	}
	if got := usertext.Failure("", buildfeatures.ErrOpenCodeGoDisabled.Error(), "opencode-go"); got != buildfeatures.OpenCodeGoDisabledMessage {
		t.Fatal("failure guidance lost", got)
	}
}

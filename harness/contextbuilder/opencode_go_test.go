package contextbuilder

import (
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

// OpenCode Go caches the appended prefix and reports input usage including
// cached tokens (docs/opencode-go.md, 2026-11-02 probe), so it uses the same
// append-only context as Codex.
func TestAppendOnlyContextIncludesOpenCodeGo(t *testing.T) {
	if !newContextFixture(t, "opencode-go", contextengine.Config{}).b.UsesAppendOnlyContext() {
		t.Fatal("OpenCode Go context is not append-only")
	}
}

// Mirrors TestMeasuredInputTakesPrecedenceOverByteEstimate.
func TestOpenCodeGoMeasuredInputTakesPrecedenceOverByteEstimate(t *testing.T) {
	f := newContextFixture(t, "opencode-go", contextengine.Config{})
	f.b.SetContextRuntime(sessionstore.RuntimeSelection{Provider: "opencode-go", Model: "deepseek-v4.1-flash", ContextWindow: 200000}, true)
	f.user(t, "task", "Continue this task")
	f.item(t, sessionstore.ItemTurn, session.Turn{ID: "turn", Type: session.TurnRegular})
	f.item(t, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "turn", Response: llm.Response{Usage: llm.Usage{InputTokens: 2000}, Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: strings.Repeat("x", 210000)}}}}})
	if err := f.b.PrepareContext(t.Context(), nil, llm.RequestOptions{}, func(r sessionstore.ContextRecord) error { f.b.ApplyContext(r); return nil }, false); err != nil {
		t.Fatal(err)
	}
	built, err := f.b.Build()
	if err != nil {
		t.Fatal("byte estimate overrode actual usage", err)
	}
	if built.Report.Context.EstimatedInputTokens <= built.Report.Context.Budget.Input {
		t.Fatal("fixture did not exceed the byte estimate budget")
	}
	f.item(t, sessionstore.ItemTurn, session.Turn{ID: "unreported", Type: session.TurnRegular})
	f.item(t, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "unreported", Response: llm.Response{}})
	if _, err := f.b.Build(); !contextengine.IsBudgetError(err) {
		t.Fatal("missing latest usage did not restore estimation")
	}
}

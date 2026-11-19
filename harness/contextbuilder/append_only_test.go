package contextbuilder

import (
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func TestMeasuredInputTakesPrecedenceOverTokenEstimate(t *testing.T) {
	t.Parallel()
	f := newContextFixture(t, "openai-codex", contextengine.Config{})
	f.b.SetContextRuntime(sessionstore.RuntimeSelection{Provider: "openai-codex", Model: "synthetic-model", ContextWindow: 200000}, true)
	if err := f.b.PrepareContext(t.Context(), func(r sessionstore.ContextRecord) error { f.b.ApplyContext(r); return nil }, false); err != nil {
		t.Fatal(err)
	}
	f.user(t, "task", "Continue this task"+strings.Repeat("x", 840000))
	f.item(t, sessionstore.ItemTurn, session.Turn{ID: "turn", Type: session.TurnRegular})
	f.item(t, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "turn", Response: llm.Response{Usage: llm.Usage{InputTokens: 2000}, Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: strings.Repeat("x", 210000)}}}}})
	if err := f.b.PrepareContext(t.Context(), func(r sessionstore.ContextRecord) error { f.b.ApplyContext(r); return nil }, false); err != nil {
		t.Fatal(err)
	}
	built, err := f.b.Build()
	if err != nil {
		t.Fatal("token estimate overrode measured prefix plus delta", err)
	}
	if built.Report.Context.EstimatedInputTokens <= built.Report.Context.Budget.Input {
		t.Fatal("fixture did not exceed the token estimate budget")
	}
	// An unreported later call must not keep treating that older measured value
	// as if it described the latest request.
	f.item(t, sessionstore.ItemTurn, session.Turn{ID: "unreported", Type: session.TurnRegular})
	f.item(t, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "unreported", Response: llm.Response{}})
	if _, err := f.b.Build(); !contextengine.IsBudgetError(err) {
		t.Fatal("missing latest usage did not restore estimation")
	}
}

package contextbuilder

import (
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func TestRecordedToolProjectionUsesCallLimitAndKeepsTypedStateAfterReplay(t *testing.T) {
	f := newContextFixture(t, "openai-codex", contextengine.Config{})
	f.user(t, "task", "Inspect the output")
	f.item(t, sessionstore.ItemTurn, session.Turn{ID: "turn", Type: session.TurnRegular})
	f.item(t, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "turn", Response: llm.Response{Output: []llm.Item{{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "read", Name: "read", Arguments: `{"path":"source.go","max_output_length":100}`}}}}})
	original := "HEAD" + strings.Repeat("middle ", 100) + "TAIL"
	f.b.AddTypedToolResult(llm.ToolResult{CallID: "read", State: llm.ToolResultSucceeded, Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: original}}}, false)
	if len(f.b.pendingProjections) != 1 {
		t.Fatal("projection was not fixed at recording time")
	}
	record := f.b.pendingProjections[0]
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	var replay sessionstore.ContextRecord
	if err := json.Unmarshal(encoded, &replay); err != nil {
		t.Fatal(err)
	}
	f.b.ApplyContext(replay)
	result, err := f.b.Build()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range result.Request.Input {
		if r, ok := item.Data.(llm.ToolResult); ok && r.CallID == "read" {
			text := r.Output[0].Value
			if r.State != llm.ToolResultSucceeded || strings.Contains(text, original) || !strings.Contains(text, "HEAD") || !strings.Contains(text, "TAIL") || !strings.Contains(text, "Unit ID") {
				t.Fatal("projection lost state/head/tail/reference")
			}
			found = true
		}
	}
	unit, ok := f.b.engine.ResolveUnit(record.UnitID)
	if !found || !ok || contextengine.Text(unit.Item) != original {
		t.Fatal("canonical full result was changed")
	}
}

func TestClaudeOwnHistoryDoesNotUseCodexCompactionBudget(t *testing.T) {
	f := newContextFixture(t, "claude-code", contextengine.Config{InputBudget: 6000})
	f.turn(t, "old", "Initial task", strings.Repeat("Claude Code keeps this history. ", 3000))
	f.user(t, "new", "Next input")
	if _, err := f.b.Build(); err != nil {
		t.Fatal("local Codex budget blocked Claude's own history", err)
	}
}

func TestMeasuredInputTakesPrecedenceOverByteEstimate(t *testing.T) {
	f := newContextFixture(t, "openai-codex", contextengine.Config{})
	f.b.SetContextRuntime(sessionstore.RuntimeSelection{Provider: "openai-codex", Model: "synthetic-model", ContextWindow: 200000}, true)
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
	// An unreported later call must not keep treating that older measured value
	// as if it described the latest request.
	f.item(t, sessionstore.ItemTurn, session.Turn{ID: "unreported", Type: session.TurnRegular})
	f.item(t, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "unreported", Response: llm.Response{}})
	if _, err := f.b.Build(); !contextengine.IsBudgetError(err) {
		t.Fatal("missing latest usage did not restore estimation")
	}
}

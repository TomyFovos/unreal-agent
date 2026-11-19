package contextbuilder

import (
	"fmt"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudeagent"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

func fullTurnEstimate(b *builder) int64 {
	input := b.deriveItems()
	if b.windowPrompt != nil {
		return b.windowHeaderTokens(input[0]) + b.tokenEstimator().Text(b.windowPrompt.Render(input))
	}
	return b.tokenEstimator().Request(llm.Request{Model: b.request.Model, Tools: b.request.Tools, Input: input})
}

func TestIncrementalWindowEstimateMatchesRenderedPrompt(t *testing.T) {
	f := newContextFixture(t, claudeagent.ProviderID, contextengine.Config{})
	f.b.SetWindowPrompt(claudeagent.WindowPromptLine)
	check := func() {
		t.Helper()
		full := claudeagent.WindowPrompt(f.b.deriveItems())
		head := (contextengine.TokenEstimator{}).Request(llm.Request{Input: f.b.committedPrefix[:1], Tools: f.b.request.Tools})
		if got, want := f.b.estimateRequest(), head+f.b.tokenEstimator().Text(full); got != want {
			t.Fatalf("incremental=%d rendered=%d, prompt=%q", got, want, full)
		}
	}
	check() // the empty renderer fallback
	f.turn(t, "empty", "", "one")
	check()
	f.user(t, "mixed", " 日本語\r\n🙂 ")
	f.item(t, sessionstore.ItemTurn, session.Turn{ID: "tool", Type: session.TurnRegular})
	f.item(t, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "tool", Response: llm.Response{Output: []llm.Item{
		{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "test", Name: "Read", Arguments: `{"path":"資料.txt"}`}},
	}}})
	f.item(t, sessionstore.ItemToolCallStatus, sessionstore.ToolCallStatus{
		TurnID: "tool", CallID: "test", Status: tool.CallStatus{Error: "空\nﬁle"},
	})
	check()
	f.b.ApplyContext(sessionstore.ContextRecord{Kind: sessionstore.ContextWindowBoundary, Through: f.b.lastUnit,
		Text: "L2: 状態", Window: &sessionstore.WindowBoundary{ToolOutputExcerptChars: f.b.excerptChars()}})
	check()
	f.turn(t, "another", "two!", "three")
	check()
}

func TestIncrementalTurnEstimateMatchesFullTokenEstimateAcrossViewChanges(t *testing.T) {
	f := newContextFixture(t, "openai-codex", contextengine.Config{})
	for i := range 14 {
		switch i {
		case 3:
			f.b.ApplyContext(sessionstore.ContextRecord{Kind: "projection", UnitID: "unit-00000000000000000002", Items: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "replacement \"body\"\n"}}}})
		case 6:
			f.b.ApplyContext(sessionstore.ContextRecord{Kind: sessionstore.ContextWindowBoundary, Through: "unit-00000000000000000006", Retained: []string{"unit-00000000000000000002"}, Text: "state 日本語", Window: &sessionstore.WindowBoundary{}})
		case 9:
			f.b.SetRuntimeProvider("claude-code")
			f.b.SetWindowPrompt(claudeagent.WindowPromptLine)
			f.b.SetModel(llm.Model{ID: "other-model", ReasoningEffort: "high"})
		case 11:
			f.b.ApplyContext(sessionstore.ContextRecord{Kind: "runtime", Through: f.b.lastUnit, Provider: "openai-codex", Model: "changed"})
		case 12:
			f.b.SetPermissionPrompt("updated permission prompt")
			f.b.AddTool(llm.Tool{Type: llm.ToolFunction, Name: "test", Parameters: map[string]any{"type": "object"}})
		case 13:
			max := int64(321)
			f.b.SetModel(llm.Model{ID: f.b.request.Model.ID, MaxOutputTokens: &max, ReasoningEffort: "low"})
		}
		id := session.TurnID(fmt.Sprintf("turn-%d", i))
		f.user(t, string(id)+"-input", strings.Repeat("quotes \"\n日本語👨‍👩‍👧‍👦 ", i+1))
		f.item(t, sessionstore.ItemTurn, session.Turn{ID: id, Type: session.TurnRegular})
		want := fullTurnEstimate(f.b)
		if got := f.b.turnEstimates[id].tokens; got != want {
			t.Fatalf("turn %d: incremental=%d full tokens=%d", i, got, want)
		}
		f.b.SetHistoryRuntime(f.b.provider, f.b.request.Model.ID)
		f.item(t, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: id, Response: llm.Response{Usage: llm.Usage{InputTokens: 123, OutputTokens: 4}, Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "response"}}}}})
		if f.b.lastInput != 123 || f.b.lastEstimate.tokens != want {
			t.Fatal("replay changed the measured input anchor")
		}
	}
}

func TestHistoricalReceiptProjectionKeepsOriginalEstimate(t *testing.T) {
	for _, provider := range []string{claudeagent.ProviderID, "openai-codex"} {
		t.Run(provider, func(t *testing.T) {
			f := newContextFixture(t, provider, contextengine.Config{})
			if provider == claudeagent.ProviderID {
				f.b.SetWindowPrompt(claudeagent.WindowPromptLine)
			}
			f.user(t, "input", "Read the result")
			f.item(t, sessionstore.ItemTurn, session.Turn{ID: "call", Type: session.TurnRegular})
			f.item(t, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "call", Response: llm.Response{Output: []llm.Item{
				{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "inspect", Name: "Read", Arguments: "{}"}},
			}}})
			f.item(t, sessionstore.ItemToolCallStatus, sessionstore.ToolCallStatus{
				TurnID: "call", CallID: "inspect", Status: tool.CallStatus{Error: "original receipt"},
			})
			receipt := f.b.lastUnit
			f.user(t, "after", "  Continue 日本語  ")
			before := f.b.requestEstimate()
			if want := fullTurnEstimate(f.b); before.tokens != want {
				t.Fatalf("initial estimate=%d full=%d", before.tokens, want)
			}
			for _, text := range []string{"first replacement", "短い"} {
				f.b.ApplyContext(sessionstore.ContextRecord{Kind: "projection", UnitID: receipt, Items: []llm.Item{
					{Type: llm.ItemToolResult, Data: llm.ToolResult{CallID: "inspect", Output: []llm.ToolResultOutput{
						{Kind: llm.ToolResultText, Value: text},
					}}},
				}})
			}
			after := f.b.requestEstimate()
			if want := fullTurnEstimate(f.b); after.tokens != want || after.tokens != before.tokens {
				t.Fatalf("updated estimate=%d full=%d initial=%d", after.tokens, want, before.tokens)
			}
			if after.epoch != before.epoch || after.header != before.header {
				t.Fatal("historical replacement changed the original receipt projection")
			}
		})
	}
}

// Opaque reasoning is replay state, not tokenized JSON. Its nesting or
// ciphertext size must not manufacture an admission overflow sentinel.
func TestIncrementalTokenEstimateIgnoresOpaqueJSONOverhead(t *testing.T) {
	for _, depth := range []int{9994, 9995, 9996, 9997, 9998} {
		t.Run(fmt.Sprint(depth), func(t *testing.T) {
			f := newContextFixture(t, "openai-codex", contextengine.Config{})
			f.user(t, "input", "hello")
			before := fullTurnEstimate(f.b)
			f.item(t, sessionstore.ItemModelResponse, sessionstore.ModelResponse{Response: llm.Response{Output: []llm.Item{
				{Type: llm.ItemReasoning, Data: llm.Reasoning{Raw: []byte(strings.Repeat("[", depth) + "0" + strings.Repeat("]", depth))}},
			}}})
			if got, want := f.b.estimateRequest(), fullTurnEstimate(f.b); got != want || got != before {
				t.Fatalf("depth %d: incremental=%d full tokens=%d before=%d", depth, got, want, before)
			}
		})
	}
}

// The full-token sub-benchmark walks the complete request at every Turn,
// retaining an independent oracle for the incremental estimate.
func BenchmarkRestoreTurnEstimates(b *testing.B) {
	for _, legacy := range []bool{true, false} {
		name := "incremental"
		if legacy {
			name = "full-tokens"
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				f := newContextFixture(b, "openai-codex", contextengine.Config{})
				for i := range 2000 {
					id := session.TurnID(fmt.Sprintf("t-%d", i))
					f.user(b, string(id)+"-input", "synthetic input")
					f.item(b, sessionstore.ItemTurn, session.Turn{ID: id, Type: session.TurnRegular})
					if legacy {
						f.b.turnEstimates[id] = requestEstimate{tokens: fullTurnEstimate(f.b), header: f.b.estimate.header, epoch: f.b.estimate.epoch}
					}
					f.item(b, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: id, Response: llm.Response{Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: strings.Repeat("x", 4500)}}}}})
				}
			}
		})
	}
}

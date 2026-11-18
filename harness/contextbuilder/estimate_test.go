package contextbuilder

import (
	"fmt"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func fullTurnEstimate(b *builder) int64 {
	return b.tokenEstimator().Request(llm.Request{Model: b.request.Model, Tools: b.request.Tools, Input: b.deriveItems()})
}

func TestIncrementalTurnEstimateMatchesFullTokenEstimateAcrossViewChanges(t *testing.T) {
	f := newContextFixture(t, "openai-codex", contextengine.Config{})
	for i := range 14 {
		switch i {
		case 3:
			f.b.ApplyContext(sessionstore.ContextRecord{Kind: "projection", UnitID: "unit-00000000000000000002", Items: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "replacement \"body\"\n"}}}})
		case 6:
			f.b.ApplyContext(sessionstore.ContextRecord{Kind: "summary", Through: "unit-00000000000000000006", Retained: []string{"unit-00000000000000000002"}, Text: "summary 日本語"})
		case 9:
			f.b.SetRuntimeProvider("claude-code")
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
		if f.b.pruneInput != 123 || f.b.pruneEstimate != want {
			t.Fatal("replay changed measured pruning calibration")
		}
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
			id := f.b.lastUnit
			native := f.b.native[id]
			native.extra = []llm.Item{{Type: llm.ItemReasoning, Data: llm.Reasoning{Raw: []byte(strings.Repeat("[", depth) + "0" + strings.Repeat("]", depth))}}}
			f.b.native[id] = native
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

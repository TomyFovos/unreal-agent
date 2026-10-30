package contextbuilder

import (
	"encoding/json/jsontext"
	"fmt"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func fullTurnEstimate(b *builder) int64 {
	return contextengine.Estimate(llm.Request{Model: b.request.Model, Tools: b.request.Tools, Input: b.deriveItems()})
}

func TestIncrementalTurnEstimateMatchesFullJSONAcrossViewChanges(t *testing.T) {
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
		if got := f.b.turnEstimates[id]; got != want {
			t.Fatalf("turn %d: incremental=%d full JSON=%d", i, got, want)
		}
		f.item(t, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: id, Response: llm.Response{Usage: llm.Usage{InputTokens: 123, OutputTokens: 4}, Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "response"}}}}})
		if f.b.pruneInput != 123 || f.b.pruneEstimate != want {
			t.Fatal("replay changed measured pruning calibration")
		}
	}
}

func TestIncrementalEstimateKeepsCombinedOpaqueJSONDepthFailure(t *testing.T) {
	for _, depth := range []int{9994, 9995, 9996, 9997, 9998} {
		t.Run(fmt.Sprint(depth), func(t *testing.T) {
			f := newContextFixture(t, "openai-codex", contextengine.Config{})
			f.user(t, "input", "hello")
			id := f.b.lastUnit
			native := f.b.native[id]
			native.extra = []llm.Item{{Type: llm.ItemReasoning, Data: llm.Reasoning{Raw: jsontext.Value(strings.Repeat("[", depth) + "0" + strings.Repeat("]", depth))}}}
			f.b.native[id] = native
			got := f.b.estimateRequest()
			if want := fullTurnEstimate(f.b); got != want {
				t.Fatalf("depth %d: incremental=%d full JSON=%d", depth, got, want)
			}
		})
	}
}

// The legacy sub-benchmark is the complete pre-optimization estimate at every
// Turn. It intentionally retains that cost as a reproducible comparison oracle.
func BenchmarkRestoreTurnEstimates(b *testing.B) {
	for _, legacy := range []bool{true, false} {
		name := "incremental"
		if legacy {
			name = "full-json"
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
						f.b.turnEstimates[id] = fullTurnEstimate(f.b)
					}
					f.item(b, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: id, Response: llm.Response{Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: strings.Repeat("x", 4500)}}}}})
				}
			}
		})
	}
}

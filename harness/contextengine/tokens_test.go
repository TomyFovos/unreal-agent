package contextengine

import (
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/claudeprompt"
)

func TestProviderTokenEstimatesUseContentNotTransportBytes(t *testing.T) {
	for _, provider := range []string{"claude-code", "claude", "openai-codex", "openai", "opencode-go", "openrouter", "fireworks", "ollama"} {
		t.Run(provider, func(t *testing.T) {
			e := TokenEstimator{Provider: provider}
			request := llm.Request{Model: llm.Model{ID: strings.Repeat("model", 1000)}, Input: []llm.Item{
				{Type: llm.ItemMessage, ProviderID: strings.Repeat("metadata", 10000), Data: llm.Message{Role: llm.RoleSystem, Text: "system!!"}},
				{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "a\n\"b日本"}},
				{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: strings.Repeat("id", 10000), Name: "read", Arguments: "{}"}},
				{Type: llm.ItemToolResult, Data: llm.ToolResult{Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "12345678"}}}},
				{Type: llm.ItemReasoning, Data: llm.Reasoning{Summary: []string{"done"}, Raw: []byte(`{"type":"reasoning","encrypted_content":"` + strings.Repeat("opaque", 50000) + `"}`)}},
			}, Tools: []llm.Tool{{Name: "read", Description: "readfile", Parameters: map[string]any{"type": "object"}}}}
			want := int64(2 + 3 + 2 + 2 + 0 + 1 + 2 + 5)
			if provider == "claude-code" || provider == "claude" {
				want = e.Text(claudeprompt.Plan(request.Input).Full)
			}
			if got := e.Request(request); got != want {
				t.Fatalf("%s: tokens=%d want=%d", e.Profile(), got, want)
			}
			if JSONBytes(request) < 100000 {
				t.Fatal("fixture lacks transport overhead")
			}
		})
	}
}

func TestTokenEstimateHasNoMarginAndDoesNotFailClosedOnSchemaEncoding(t *testing.T) {
	e := TokenEstimator{Provider: "openai"}
	if e.Request(llm.Request{}) != 0 {
		t.Fatal("empty payload acquired a margin")
	}
	for _, tc := range []struct {
		text string
		want int64
	}{{"", 0}, {"abcd", 1}, {"abcde", 2}, {"日本語", 3}, {"a\n\"b", 1}} {
		if got := e.Text(tc.text); got != tc.want {
			t.Fatalf("%q: %d want %d", tc.text, got, tc.want)
		}
	}
	if got := e.Tools([]llm.Tool{{Name: "test", Parameters: map[string]any{"invalid": make(chan int)}}}); got != 1 {
		t.Fatal("schema encoding failure became a context overflow sentinel", got)
	}
}

func TestReasoningEstimateUsesReplaySummaryWithoutCiphertextSize(t *testing.T) {
	e := TokenEstimator{Provider: "openai"}
	item := llm.Item{Type: llm.ItemReasoning, Data: llm.Reasoning{Summary: []string{strings.Repeat("stale", 1000)}, Raw: []byte(`{"type":"reasoning","summary":[{"type":"summary_text","text":"replayed"}],"encrypted_content":"` + strings.Repeat("opaque", 100000) + `"}`)}}
	if got := e.Item(item); got != 2 {
		t.Fatal("canonical summary or ciphertext replaced actual replay summary", got)
	}
}

package contextbuilder

import (
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"strings"
	"testing"
)

func TestProviderProjectionKeepsOwnToolsAndOmitsForeignPrivateState(t *testing.T) {
	b := NewBuilder().(*builder)
	b.SetRuntimeProvider("openai-codex")
	b.ConfigureRuntime(llm.Model{ID: "gpt", ReasoningEffort: "medium"}, "", []llm.Tool{{Name: "read"}}, nil, false, true)
	b.AddModelResponse(llm.Response{Output: []llm.Item{{Type: llm.ItemReasoning, Data: llm.Reasoning{Raw: []byte(`{"private":"provider-private-marker"}`)}}, {Type: llm.ItemToolCall, Data: llm.ToolCall{Name: "read", CallID: "call", Arguments: `{"path":"x"}`}}}})
	b.AddToolResult("call", []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "observed result"}}, false)
	b.Commit()
	own, e := b.Build()
	if e != nil {
		t.Fatal(e)
	}
	if len(own.Request.Tools) != 1 || own.Request.Input[1].Type != llm.ItemReasoning || own.Request.Input[2].Type != llm.ItemToolCall || own.Request.Input[3].Type != llm.ItemToolResult {
		t.Fatal("Codex's own tool loop was degraded")
	}
	b.SetRuntimeProvider("claude-code")
	b.ConfigureRuntime(llm.Model{ID: "sonnet", ReasoningEffort: "high"}, "", nil, nil, true, true)
	foreign, e := b.Build()
	if e != nil {
		t.Fatal(e)
	}
	for _, item := range foreign.Request.Input {
		if item.Type != llm.ItemMessage {
			t.Fatal("Claude received tool/private state")
		}
		m := item.Data.(llm.Message)
		if strings.Contains(m.Text, "provider-private-marker") {
			t.Fatal("foreign reasoning leaked")
		}
	}
	if len(foreign.Request.Tools) != 0 {
		t.Fatal("Claude received tools")
	}
	b.SetRuntimeProvider("openai-codex")
	b.ConfigureRuntime(llm.Model{ID: "gpt", ReasoningEffort: "medium"}, "", []llm.Tool{{Name: "read"}}, nil, false, true)
	restored, _ := b.Build()
	if restored.Request.Input[2].Type != llm.ItemToolCall || restored.Request.Input[3].Type != llm.ItemToolResult {
		t.Fatal("switching back rewrote canonical context")
	}
	// Resume starts with the latest request provider but attributes replay using
	// canonical runtime records; it must not label an old provider's reasoning
	// as belonging to the active provider.
	replay := NewBuilder().(*builder)
	replay.SetRuntimeProvider("claude-code")
	replay.ConfigureRuntime(llm.Model{ID: "sonnet"}, "", nil, nil, true, true)
	replay.SetHistoryProvider("openai-codex")
	replay.AddModelResponse(llm.Response{Output: own.Request.Input[1:3]})
	replay.AddToolResult("call", []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "observed result"}}, false)
	r, _ := replay.Build()
	for _, item := range r.Request.Input {
		if item.Type != llm.ItemMessage {
			t.Fatal("replay misattributed provider context")
		}
	}
}

func TestProviderSwitchPreservesHostLifecycleGuidance(t *testing.T) {
	for _, lifecycle := range []string{"interactive", "child"} {
		t.Run(lifecycle, func(t *testing.T) {
			b := NewBuilder().(*builder)
			if err := b.SetLifecycle(lifecycle); err != nil {
				t.Fatal(err)
			}
			for _, textOnly := range []bool{true, false, true} {
				b.ConfigureRuntime(llm.Model{ID: "configured"}, "caller instructions", nil, nil, textOnly, true)
				request, err := b.Build()
				if err != nil {
					t.Fatal(err)
				}
				prompt := request.Request.Input[0].Data.(llm.Message).Text
				if strings.Contains(prompt, oneShotEnding) || strings.Contains(prompt, textOnlyEnding) || !strings.Contains(prompt, "caller instructions") {
					t.Fatal("provider change lost Host lifecycle or caller guidance")
				}
				if lifecycle == "interactive" && !strings.Contains(prompt, "The host keeps the session alive until an explicit stop.") {
					t.Fatal("runtime change restored one-shot prompt")
				}
				if lifecycle == "child" {
					if textOnly && (!strings.Contains(prompt, "Unreal records its completion") || strings.Contains(prompt, "Complete delegated work only with the explicit Finish tool")) {
						t.Fatal("text-only child was instructed to call an unavailable tool")
					}
					if !textOnly && !strings.Contains(prompt, "Complete delegated work only with the explicit Finish tool") {
						t.Fatal("Codex child lost Finish guidance")
					}
				}
			}
		})
	}
}

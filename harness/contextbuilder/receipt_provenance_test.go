package contextbuilder

import (
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

func portableReceiptBuilder(t *testing.T, bounded bool) *builder {
	t.Helper()
	b := NewBuilder().(*builder)
	if bounded {
		if _, err := EnableCompaction(b, contextengine.Config{InputBudget: 24576}, contextengine.Constraints{}); err != nil {
			t.Fatal(err)
		}
	}
	b.SetRuntimeProvider("claude-code")
	b.ConfigureRuntime(llm.Model{ID: "claude-configured-a"}, "", nil, nil, false, true)
	if err := b.AddExternalInput(inbox.Input{ID: "task", Kind: inbox.InputExternal, Payload: []byte(`"Inspect the prior receipts."`)}); err != nil {
		t.Fatal(err)
	}
	b.SetHistoryProvider("openai-codex")
	b.AddModelResponse(llm.Response{Output: []llm.Item{{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "same-call", Name: "read", Arguments: `{"path":"fixture.txt"}`}}}})
	return b
}

func TestBuilderPortableReceiptKeepsUnadoptedSecretsOut(t *testing.T) {
	b := portableReceiptBuilder(t, true)
	b.SetHistoryItem(sessionstore.Item{Kind: sessionstore.ItemToolCallStatus, Sequence: 3, Data: sessionstore.ToolCallStatus{CallID: "same-call", Status: tool.CallStatus{WaitingFor: []operation.ID{"op"}}, Operations: []operation.Operation{{ID: "op", Status: operation.StatusCompleted}}}})
	b.AddToolResult("same-call", []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "Authorization: Bearer SYNTHETIC_UNADOPTED_SECRET"}}, false)
	built, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []any{built.Request, built.Package, built.Report, ContextDebug(built)} {
		encoded, err := json.Marshal(value)
		if err != nil || strings.Contains(string(encoded), "SYNTHETIC_UNADOPTED_SECRET") {
			t.Fatal("secret entered context index, export or diagnostics", err)
		}
	}
}

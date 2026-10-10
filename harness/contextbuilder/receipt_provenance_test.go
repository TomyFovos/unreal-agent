package contextbuilder

import (
	"encoding/json/v2"
	"errors"
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

func TestBuilderReceiptProvenanceLegacyAndBoundedUpdates(t *testing.T) {
	for _, bounded := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "bounded"}[bounded], func(t *testing.T) {
			b := portableReceiptBuilder(t, bounded)
			b.AddToolResult("same-call", nil, true)
			running, err := b.Build()
			if err != nil {
				t.Fatal(err)
			}
			origins, err := llm.ReceiptProjections(running.Request)
			if err != nil || len(origins) != 1 {
				t.Fatal("running receipt has no typed origin", err)
			}
			for _, receipt := range origins {
				if receipt.CallID != "same-call" || len(receipt.Output) != 1 || receipt.Output[0].Value != ToolCallRunningPayload {
					t.Fatal("origin lost running receipt")
				}
			}
			b.AddToolResult("same-call", []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "adopted-terminal"}}, false)
			terminal, err := b.Build()
			if err != nil {
				t.Fatal(err)
			}
			origins, err = llm.ReceiptProjections(terminal.Request)
			if err != nil || len(origins) != 1 {
				t.Fatal("terminal receipt has no typed origin", err)
			}
			for position, receipt := range origins {
				if receipt.CallID != "same-call" || len(receipt.Output) != 1 || receipt.Output[0].Value != "adopted-terminal" {
					t.Fatal("running origin was reused for final")
				}
				public, ok := contextengine.PublicItem(terminal.Request.Input[position])
				if !ok {
					t.Fatal("public quote rejected")
				}
				// Eligibility/indexing must strip request-local origins.
				plain, err := llm.ReceiptProjections(llm.Request{Input: []llm.Item{public}})
				if err != nil || len(plain) != 0 {
					t.Fatal("internal metadata survived public/index projection", err)
				}
			}
			encoded, _ := json.Marshal(terminal.Request)
			rebuilt, err := b.Build()
			if err != nil {
				t.Fatal(err)
			}
			encodedAgain, _ := json.Marshal(rebuilt.Request)
			if string(encoded) != string(encodedAgain) {
				t.Fatal("repeated Build changed public receipt projection")
			}
			// Combining a previous manifest with later projected items is an
			// inconsistency even when CallID and positions appear unchanged.
			running.Request.Input = terminal.Request.Input
			_, err = llm.ReceiptProjections(running.Request)
			var typed *llm.ReceiptProjectionError
			if !errors.As(err, &typed) {
				t.Fatal("running/final provenance mismatch accepted")
			}
		})
	}
}

func TestBuilderReceiptProvenanceKeepsUnadoptedSecretsOut(t *testing.T) {
	b := portableReceiptBuilder(t, true)
	b.SetHistoryItem(sessionstore.Item{Kind: sessionstore.ItemToolCallStatus, Sequence: 3, Data: sessionstore.ToolCallStatus{CallID: "same-call", Status: tool.CallStatus{WaitingFor: []operation.ID{"op"}}, Operations: []operation.Operation{{ID: "op", Status: operation.StatusCompleted}}}})
	b.AddToolResult("same-call", []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "Authorization: Bearer SYNTHETIC_UNADOPTED_SECRET"}}, false)
	built, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	origins, err := llm.ReceiptProjections(built.Request)
	if err != nil || len(origins) != 1 {
		t.Fatal("redacted receipt has no safe typed origin", err)
	}
	for _, receipt := range origins {
		if len(receipt.Output) != 1 || !strings.Contains(receipt.Output[0].Value, `"state":"completed"`) || strings.Contains(receipt.Output[0].Value, "SYNTHETIC_UNADOPTED_SECRET") {
			t.Fatal("origin was attached to raw rather than adopted receipt")
		}
	}
	for _, value := range []any{built.Request, built.Package, built.Report, ContextDebug(built)} {
		encoded, err := json.Marshal(value)
		if err != nil || strings.Contains(string(encoded), "SYNTHETIC_UNADOPTED_SECRET") || strings.Contains(string(encoded), "receiptProjection") {
			t.Fatal("secret/provenance entered context index, export or diagnostics", err)
		}
	}
}

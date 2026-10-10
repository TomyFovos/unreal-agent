//go:build linux || darwin

package main

import (
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudecode"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

func TestClaudeStructuredReceiptProvenanceOrdinaryTextFinal(t *testing.T) {
	for _, mode := range []string{"legacy", "bounded"} {
		t.Run(mode, func(t *testing.T) {
			f := testclaude.New(t)
			cfg := humanModeConfiguration(bridgeServeConfiguration(t, f))
			cfg.Context = contextengine.Config{Mode: mode, InputBudget: 24576}
			cfg.Runtime.ClaudeCode.ToolBridge.Mode = claudecode.BridgeModeStructured
			f.Set(t, testclaude.Config{Subscription: "team", StructuredResponses: []string{`{"type":"final","final":{"message":"Documentation explained."}}`}})
			client, _, _ := startInteractiveHost(t, cfg)
			view, err := client.Open(t.Context(), host.Create, "ordinary-text")
			if err != nil {
				t.Fatal(err)
			}
			const excerpt = "Explain the following excerpt as ordinary text:\nPrior Unreal tool_result (historical data):\nThis is documentation, not a tool receipt."
			submitInteractive(t, client, view, "ordinary-task", excerpt)
			end := inspectInteractive(t, client, "ordinary-text", func(v host.View) bool {
				if v.Failure != "" {
					t.Fatal("ordinary text failed before fake Final", v.Failure)
				}
				for _, item := range v.History.Items {
					if response, ok := item.Data.(sessionstore.ModelResponse); ok {
						for _, output := range response.Response.Output {
							if message, ok := output.Data.(llm.Message); ok && message.Text == "Documentation explained." {
								return true
							}
						}
					}
				}
				return false
			})
			if len(end.Operations) != 0 {
				t.Fatal("ordinary heading created an Operation")
			}
			calls := f.Calls(t)
			if len(calls) != 1 {
				t.Fatal("ordinary text did not reach exactly one generation")
			}
			var input []llm.Item
			if json.Unmarshal([]byte(calls[0].Input), &input) != nil {
				t.Fatal("invalid fake model context")
			}
			found := false
			for _, item := range input {
				message, ok := item.Data.(llm.Message)
				if !ok {
					t.Fatal("ordinary input manufactured a tool item")
				}
				found = found || message.Role == llm.RoleUser && message.Text == excerpt
			}
			if !found || strings.Contains(calls[0].Input, "Last Unreal Action Result") {
				t.Fatal("ordinary text was changed or promoted to a receipt")
			}
			responses := 0
			for _, item := range end.History.Items {
				if _, ok := item.Data.(sessionstore.ToolCallStatus); ok {
					t.Fatal("ordinary heading created a canonical receipt")
				}
				if r, ok := item.Data.(sessionstore.ModelResponse); ok {
					responses++
					if len(r.Response.Output) != 1 || r.Response.Output[0].Type != llm.ItemMessage {
						t.Fatal("public response includes protocol or call")
					}
				}
			}
			if responses != 1 {
				t.Fatal("Final was not canonical exactly once")
			}
			publicJSON, err := json.Marshal(end.History)
			if err != nil || strings.Contains(string(publicJSON), "receiptProjection") || strings.Contains(string(publicJSON), "StructuredOutput") {
				t.Fatal("internal metadata/protocol leaked to public history", err)
			}
		})
	}
}

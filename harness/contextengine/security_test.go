package contextengine

import (
	"reflect"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

func TestPublicItemRedactsToolContentWithoutDroppingIt(t *testing.T) {
	const code = "def read(token: str):\n    secret = load()\n    return state_store()\n"
	for _, value := range []string{code, "sk-synthetic-credential", "Bearer synthetic-credential", "eyJsynthetic.value.signature", "private_key=-----BEGIN PRIVATE KEY-----\nsynthetic-credential\n-----END PRIVATE KEY-----"} {
		for _, original := range []llm.Item{
			{Type: llm.ItemToolCall, ProviderID: "native-id", Data: llm.ToolCall{CallID: "call", Name: "read", Arguments: "read source.py " + value}},
			{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "call", Name: "read " + value, Arguments: "source.py"}},
			{Type: llm.ItemToolResult, Data: llm.ToolResult{CallID: "call", State: llm.ToolResultFailed, Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "source.py\n" + value}, {Kind: llm.ToolResultText, Value: "keep second output"}}}},
		} {
			before := Text(original)
			public, ok := PublicItem(original)
			if !ok || public.Type != original.Type || public.ProviderID != "" || Sensitive(Text(public)) || strings.Contains(Text(public), "synthetic-credential") || !strings.Contains(Text(public), "[REDACTED]") || !strings.Contains(Text(public), "source.py") {
				t.Fatalf("public tool content missing or unsafe: %#v", public)
			}
			if Text(original) != before {
				t.Fatal("canonical source was mutated")
			}
			if value == code && !strings.Contains(Text(public), "return state_store()") {
				t.Fatal("surrounding code was lost")
			}
			if result, ok := public.Data.(llm.ToolResult); ok && (result.State != llm.ToolResultFailed || len(result.Output) != 2 || result.Output[1].Value != "keep second output") {
				t.Fatal("receipt state or other output changed")
			}
		}
	}
}

func TestPublicItemStillExcludesPrivateSources(t *testing.T) {
	for _, args := range []string{`{"path":"/synthetic/.codex/auth.json"}`, `{"path":"/synthetic/.claude/.credentials.json"}`, `{"env":{"NAME":"value"}}`, `{"environment":{}}`, `{"credentials":{}}`, `{"authentication":{}}`, `{"command":"printenv"}`, `{"command":"set -o posix"}`, `{"command":"declare -x"}`} {
		if _, ok := PublicItem(llm.Item{Type: llm.ItemToolCall, Data: llm.ToolCall{Name: "read", Arguments: args}}); ok {
			t.Fatalf("private source was retained: %s", args)
		}
	}
	ordinary := llm.Item{Type: llm.ItemToolCall, Data: llm.ToolCall{Name: "read", CallID: "call", Arguments: `{"path":"state_store.py"}`}}
	if got, ok := PublicItem(ordinary); !ok || !reflect.DeepEqual(got, ordinary) {
		t.Fatal("ordinary legacy tool call changed")
	}
}

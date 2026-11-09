package contextengine

import (
	"reflect"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

func TestPublicItemMeasurementUsesExistingEligibilityAndPayloadBytes(t *testing.T) {
	for _, tc := range []struct {
		item          llm.Item
		reason        string
		call          string
		before, after int64
	}{
		{llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "日本語 token=synthetic-private"}}, PartialRedaction, "", int64(len("日本語 token=synthetic-private")), int64(len("日本語 [REDACTED]"))},
		{llm.Item{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "c", Name: "read", Arguments: `{"command":"printenv"}`}}, PrivateSource, "c", int64(len("read") + len(`{"command":"printenv"}`)), 0},
		{llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: "unsupported", Text: "body"}}, NonPublic, "", 4, 0},
		{llm.Item{Type: llm.ItemToolResult, Data: llm.ToolResult{CallID: "r", Output: []llm.ToolResultOutput{{Kind: llm.ToolResultImage, Value: "token=synthetic-private"}}}}, PartialRedaction, "r", int64(len("token=synthetic-private")), int64(len("[REDACTED]"))},
		{llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "safe"}}, "", "", 0, 0},
	} {
		want, eligible := PublicItem(tc.item)
		got, safe, r := PublicItemWithRedaction(tc.item)
		if !reflect.DeepEqual(want, got) || safe != eligible {
			t.Fatal("measurement changed projection")
		}
		if tc.reason == "" {
			if r != nil {
				t.Fatal(r)
			}
			continue
		}
		if r == nil || *r != (llm.ContextRedaction{Reason: tc.reason, CallID: tc.call, BeforeBytes: tc.before, AfterBytes: tc.after}) {
			t.Fatalf("%+v", r)
		}
	}
}

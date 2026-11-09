package sessionstore

import (
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

func TestTelemetryRecordsRoundTripAndLegacyRemainReadable(t *testing.T) {
	at := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	for _, record := range []HostRecord{
		{Version: 1, Kind: HostActivity, Activity: &llm.ActivityRecord{Kind: "context_redaction", ID: "unit-1", Redaction: &llm.ContextRedaction{Reason: "partial_redaction", CallID: "call-1", BeforeBytes: 100, AfterBytes: 30}}},
		{Version: 1, Kind: HostActivity, Activity: &llm.ActivityRecord{Kind: "host_start", Revision: "revision", StartedAt: at}},
		{Version: 1, Kind: HostActivity, Activity: &llm.ActivityRecord{Kind: "native_bash", ID: "shell", StartedAt: at, EndedAt: at.Add(time.Second)}},
		{Version: 1, Kind: HostActivity, Activity: &llm.ActivityRecord{Kind: "child_start", Template: "worker", Model: "model", Effort: "high", StartedAt: at}},
		{Version: 1, Kind: HostContext, Context: &ContextRecord{Kind: "provider_conversation", Conversation: &llm.ConversationRecord{Provider: "claude", Phase: "compact", StartedAt: at, EndedAt: at.Add(time.Minute), Interrupted: true, InterruptReason: "user_input", Usage: &llm.Usage{InputTokens: 100, Unknown: []llm.UsageField{llm.UsageReasoning}}}}},
		{Version: 1, Kind: HostContext, Context: &ContextRecord{Kind: "provider_conversation", Conversation: &llm.ConversationRecord{Provider: "claude", Compacted: true, BeforeTokens: 250_000, AfterTokens: 12_000}}},
	} {
		item := Item{Kind: ItemHostRecord, Data: record}
		data, err := json.Marshal(item)
		if err != nil {
			t.Fatal(err)
		}
		var decoded Item
		if err = json.Unmarshal(data, &decoded); err != nil {
			t.Fatal(err)
		}
		again, err := json.Marshal(decoded)
		if err != nil || string(data) != string(again) {
			t.Fatal("round-trip lost fields", err)
		}
		if c := record.Context; c != nil && c.Conversation != nil && c.Conversation.AfterTokens != 0 {
			if got := decoded.Data.(HostRecord).Context.Conversation; got.AfterTokens != 12_000 || got.InputKnown || got.InputTokens != 0 {
				t.Fatal("post_tokens changed actual input", got)
			}
		}
	}
	var legacy Item
	if err := json.Unmarshal([]byte(`{"Kind":"host_record","Data":{"Version":1,"Kind":"context","Context":{"Kind":"provider_conversation","Conversation":{"Provider":"claude","Compacted":true,"InputTokens":12000,"InputKnown":true}}}}`), &legacy); err != nil {
		t.Fatal(err)
	}
	c := legacy.Data.(HostRecord).Context.Conversation
	if c.Usage != nil || !c.StartedAt.IsZero() || c.InterruptReason != "" || c.InputTokens != 12000 || c.AfterTokens != 0 {
		t.Fatal("legacy fields changed", c)
	}
	data, err := json.Marshal(legacy)
	if err != nil || strings.Contains(string(data), "AfterTokens") {
		t.Fatal("zero AfterTokens must be omitted", string(data), err)
	}
}

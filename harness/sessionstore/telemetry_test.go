package sessionstore

import (
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

func TestTelemetryRecordsRoundTripAndLegacyRemainReadable(t *testing.T) {
	at := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	for _, record := range []HostRecord{
		{Version: 1, Kind: HostActivity, Activity: &llm.ActivityRecord{Kind: "host_start", Revision: "revision", StartedAt: at}},
		{Version: 1, Kind: HostActivity, Activity: &llm.ActivityRecord{Kind: "native_bash", ID: "shell", StartedAt: at, EndedAt: at.Add(time.Second)}},
		{Version: 1, Kind: HostActivity, Activity: &llm.ActivityRecord{Kind: "child_start", Template: "worker", Model: "model", Effort: "high", StartedAt: at}},
		{Version: 1, Kind: HostContext, Context: &ContextRecord{Kind: "provider_conversation", Conversation: &llm.ConversationRecord{Provider: "claude", Phase: "compact", StartedAt: at, EndedAt: at.Add(time.Minute), Interrupted: true, InterruptReason: "user_input", Usage: &llm.Usage{InputTokens: 100, Unknown: []llm.UsageField{llm.UsageReasoning}}}}},
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
	}
	var legacy Item
	if err := json.Unmarshal([]byte(`{"Kind":"host_record","Data":{"Version":1,"Kind":"context","Context":{"Kind":"provider_conversation","Conversation":{"Provider":"claude","Compacted":true,"InputTokens":12000,"InputKnown":true}}}}`), &legacy); err != nil {
		t.Fatal(err)
	}
	c := legacy.Data.(HostRecord).Context.Conversation
	if c.Usage != nil || !c.StartedAt.IsZero() || c.InterruptReason != "" || c.InputTokens != 12000 {
		t.Fatal("legacy fields changed", c)
	}
}

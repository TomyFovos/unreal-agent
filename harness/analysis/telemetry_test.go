package analysis

import (
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func TestTelemetryAppearsInAnalyzeAndExports(t *testing.T) {
	a := New("telemetry")
	at := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	for _, activity := range []llm.ActivityRecord{
		{Kind: "host_start", StartedAt: at, Revision: "test-revision"},
		{Kind: "child_start", ID: "child-handle", StartedAt: at, Template: "worker-test", Model: "test-model", Effort: "high"},
		{Kind: "native_bash", ID: "shell-1", StartedAt: at, EndedAt: at.Add(3 * time.Second)},
		{Kind: "model_retry", StartedAt: at, Reason: "connection_error"},
	} {
		a.Apply(host.HistoryItem{Data: sessionstore.HostRecord{Version: 1, Kind: sessionstore.HostActivity, Activity: &activity}})
	}
	u := llm.Usage{InputTokens: 100, OutputTokens: 10, Unknown: []llm.UsageField{llm.UsageReasoning}}
	c := llm.ConversationRecord{Provider: "claude", Phase: "compact", Interrupted: true, InterruptReason: "user_input", StartedAt: at, EndedAt: at.Add(time.Minute), Usage: &u}
	a.Apply(host.HistoryItem{Data: sessionstore.HostRecord{Version: 1, Kind: sessionstore.HostContext, Context: &sessionstore.ContextRecord{Kind: "provider_conversation", Conversation: &c}}})
	a.Apply(host.HistoryItem{RecordedAt: at.Add(10 * time.Second), Data: sessionstore.ModelResponse{TurnID: session.TurnID("t"), Response: llm.Response{Output: []llm.Item{{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "shell-1", Name: "claude_code"}}}}}})
	a.Apply(host.HistoryItem{RecordedAt: at.Add(10 * time.Second), Data: sessionstore.ToolCallStatus{TurnID: session.TurnID("t"), CallID: "shell-1"}})
	r := a.Snapshot(at, false, false, nil, nil)
	if len(r.Activities) != 4 || len(r.Compactions) != 1 || r.Usage.Input != 100 || r.Errors.Provider != 1 {
		t.Fatal(r)
	}
	if !r.Tools.Elapsed.Known || r.Tools.Elapsed.Value != 3*time.Second {
		t.Fatal("native duration replaced with receipt timing", r.Tools.Elapsed)
	}
	for _, format := range []string{"json", "markdown"} {
		data, err := Encode(r, format)
		if err != nil {
			t.Fatal(err)
		}
		text := strings.ReplaceAll(string(data), `\_`, "_")
		for _, want := range []string{"test-revision", "worker-test", "test-model", "high", "native_bash", "user_input", "reasoning"} {
			if !strings.Contains(text, want) {
				t.Fatalf("%s missing %s", format, want)
			}
		}
	}
	if text := strings.Join(Lines(r, "Context", true), "\n"); !strings.Contains(text, "elapsed=1m0s") || !strings.Contains(text, "reasoning     unknown") {
		t.Fatal(text)
	}
	// Old records retain unknown timestamps and unknown compact usage.
	legacy := Report{Compactions: []llm.ConversationRecord{{Provider: "claude", Compacted: true}}}
	if text := strings.Join(TelemetryLines(legacy), "\n"); !strings.Contains(text, "start=unknown") || !strings.Contains(text, "compact usage unknown") {
		t.Fatal(text)
	}
}

func TestTurnTotalUsageIsVisibleWithoutDoubleCountingResponses(t *testing.T) {
	a := New("turn-total")
	a.Apply(host.HistoryItem{Data: sessionstore.ModelResponse{Response: llm.Response{Usage: llm.Usage{InputTokens: 100, OutputTokens: 20, Unknown: []llm.UsageField{llm.UsageReasoning}}}}})
	a.Apply(host.HistoryItem{Data: sessionstore.HostRecord{Version: 1, Kind: sessionstore.HostActivity, Activity: &llm.ActivityRecord{Kind: "claude_usage", Usage: &llm.Usage{InputTokens: 100, OutputTokens: 20, ReasoningTokens: 7}}}})
	r := a.Snapshot(time.Now(), false, false, nil, nil)
	if r.Usage.Input != 100 || r.Usage.Output != 20 {
		t.Fatal("turn total counted twice", r.Usage)
	}
	text := strings.Join(Lines(r, "usage", true), "\n")
	if !strings.Contains(text, "turn total") || !strings.Contains(text, "reasoning     7") {
		t.Fatal("result reasoning missing", text)
	}
}

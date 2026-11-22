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
	for _, row := range []llm.ActivityRecord{
		{Kind: "claude_usage", Model: "claude-opus-5-5", Usage: &llm.Usage{InputTokens: 125, OutputTokens: 26, ReasoningTokens: 7}},
		{Kind: "provider_usage", Model: "claude-opus-5-5", Usage: &llm.Usage{InputTokens: 100, OutputTokens: 20, ReasoningTokens: 3}},
		{Kind: "provider_usage", Model: "claude-sonnet-5-5", Usage: &llm.Usage{InputTokens: 25, OutputTokens: 6, ReasoningTokens: 4}},
	} {
		row.Provider, row.ID = "claude-code", "turn"
		a.Apply(host.HistoryItem{Data: sessionstore.HostRecord{Version: 1, Kind: sessionstore.HostActivity, Activity: &row}})
	}
	r := a.Snapshot(time.Now(), false, false, nil, nil)
	if r.Usage.Input != 100 || r.Usage.Output != 20 {
		t.Fatal("turn total counted twice", r.Usage)
	}
	text := strings.Join(Lines(r, "usage", true), "\n")
	for _, want := range []string{"turn total", "reasoning     7", "claude-opus-5-5 / input 100 / cached 0 / write 0 / reasoning 3 / output 20", "claude-sonnet-5-5 / input 25 / cached 0 / write 0 / reasoning 4 / output 6"} {
		if strings.Count(text, want) != 1 {
			t.Fatalf("want %q once in:\n%s", want, text)
		}
	}
}

func TestAgentToolDetailsKeepNestedCallsOutOfTopLevelCounts(t *testing.T) {
	a := New("agent-tools")
	a.Apply(host.HistoryItem{Data: sessionstore.ModelResponse{TurnID: "turn", Response: llm.Response{Output: []llm.Item{{
		Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "agent", Name: "claude_code"},
		ToolObservations: []llm.ToolObservation{
			{ParentCallID: "agent", Type: llm.ItemToolCall, Tool: "Read", CallID: "read"},
			{ParentCallID: "agent", Type: llm.ItemToolResult, CallID: "read", Text: "private nested output"},
		},
	}}}}})
	r := a.Snapshot(time.Now(), false, false, nil, nil)
	text := strings.Join(Lines(r, "tools", true), "\n")
	if r.Tools.Calls != 1 || len(r.NestedTools) != 1 || r.NestedTools[0] != (NestedTool{Name: "Read", Calls: 1}) ||
		!strings.Contains(text, "Read / 1") || strings.Contains(text, "private nested output") {
		t.Fatal(text)
	}
}

func TestCompactionAfterTokensAppearInAnalyzeAndExports(t *testing.T) {
	r := Report{Compactions: []llm.ConversationRecord{{Provider: "claude", Compacted: true, BeforeTokens: 250_000, AfterTokens: 12_000, InputKnown: true, InputTokens: 14_000}}}
	if text := strings.Join(Lines(r, "Context", true), "\n"); !strings.Contains(text, "before=250000 after=12000") {
		t.Fatal(text)
	}
	for _, format := range []string{"json", "markdown"} {
		data, err := Encode(r, format)
		if err != nil {
			t.Fatal(err)
		}
		want := "after=12000"
		if format == "json" {
			want = `"AfterTokens": 12000`
		}
		if !strings.Contains(string(data), want) {
			t.Fatalf("%s lost post_tokens: %s", format, data)
		}
	}
	r.Compactions[0].AfterTokens = 0
	if text := strings.Join(TelemetryLines(r), "\n"); !strings.Contains(text, "after=14000") {
		t.Fatal("legacy compact size missing", text)
	}
	r.Compactions[0].InputKnown = false
	if text := strings.Join(TelemetryLines(r), "\n"); !strings.Contains(text, "after=unknown") {
		t.Fatal("invented post_tokens", text)
	}
}

func TestRedactionCountsAndBytesAppearInAnalyzeAndExports(t *testing.T) {
	a := New("redactions")
	for _, r := range []llm.ContextRedaction{
		{Reason: "partial_redaction", CallID: "call-1", BeforeBytes: 100, AfterBytes: 40},
		{Reason: "partial_redaction", CallID: "call-2", BeforeBytes: 200, AfterBytes: 60},
		{Reason: "private_source", CallID: "call-3", BeforeBytes: 400, AfterBytes: 0},
		{Reason: "non_public", CallID: "call-4", BeforeBytes: 500, AfterBytes: 30},
	} {
		activity := llm.ActivityRecord{Kind: "context_redaction", Redaction: &r}
		a.Apply(host.HistoryItem{Data: sessionstore.HostRecord{Version: 1, Kind: sessionstore.HostActivity, Activity: &activity}})
	}
	r := a.Snapshot(time.Now(), false, false, nil, nil)
	if len(r.Redactions) != 3 || r.Redactions[0] != (RedactionTotals{Reason: "partial_redaction", Count: 2, BeforeBytes: 300, AfterBytes: 100}) {
		t.Fatal(r.Redactions)
	}
	for _, format := range []string{"context", "markdown", "json"} {
		var text string
		if format == "context" {
			text = strings.Join(Lines(r, format, true), "\n")
		} else {
			data, err := Encode(r, format)
			if err != nil {
				t.Fatal(err)
			}
			text = string(data)
		}
		text = strings.ReplaceAll(text, `\_`, "_")
		for _, want := range []string{"partial_redaction", "private_source", "non_public", "call-1", "before_bytes", "after_bytes"} {
			if !strings.Contains(text, want) {
				t.Fatalf("%s missing %s", format, want)
			}
		}
		if format != "json" && !strings.Contains(text, "count=2 before_bytes=300 after_bytes=100") {
			t.Fatal(text)
		}
	}
	legacy := New("old").Snapshot(time.Now(), false, false, nil, nil)
	if len(legacy.Redactions) != 0 {
		t.Fatal("invented historical measurements", legacy.Redactions)
	}
}

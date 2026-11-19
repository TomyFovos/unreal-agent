package claudeagent

import (
	"context"
	"encoding/json/v2"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/tool/observed"
)

func TestStartupTimingRecordsEachStageOnce(t *testing.T) {
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	var records []llm.ActivityRecord
	opt := llm.RequestOptions{Identity: llm.RequestIdentity{TurnID: "turn"}, RecordActivity: func(_ context.Context, r llm.ActivityRecord) error { records = append(records, r); return nil }}
	round := newTurn(t.Context(), func() {}, opt, &writer{w: io.Discard}, observed.NewResults(), &approver{}, t.TempDir(), "s")
	round.now = func() time.Time { return now }
	round.startup.process = now
	now = now.Add(time.Second)
	round.handle(frame{Type: "system", Subtype: "init"})
	parent := "child"
	round.handle(frame{Type: "stream_event", ParentToolUseID: &parent, Event: []byte(`{"type":"message_start"}`)})
	now = now.Add(2 * time.Second)
	round.handle(frame{Type: "stream_event", Event: []byte(`{"type":"message_start"}`)})
	round.handle(frame{Type: "stream_event", Event: []byte(`{"type":"content_block_delta","delta":{"type":"thinking_delta","text":"thinking"}}`)})
	now = now.Add(3 * time.Second)
	round.handle(frame{Type: "stream_event", Event: []byte(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"body"}}`)})
	round.handle(frame{Type: "system", Subtype: "init"})
	round.handle(frame{Type: "stream_event", Event: []byte(`{"type":"message_start"}`)})
	round.handle(frame{Type: "assistant", Message: []byte(`{"content":[{"type":"text","text":"body"}]}`)})
	if len(records) != 3 {
		t.Fatal(records)
	}
	for i, reason := range []string{"process_to_init", "init_to_message_start", "message_start_to_text"} {
		if r := records[i]; r.Kind != "claude_startup" || r.ID != "turn" || r.Reason != reason || r.EndedAt.Sub(r.StartedAt) != time.Duration(i+1)*time.Second {
			t.Fatal(records)
		}
	}
}

func TestClaudeReasoningUsageKnownAndUnknown(t *testing.T) {
	for _, field := range []string{"", "reasoning_tokens", "thinking_tokens"} {
		t.Run(field, func(t *testing.T) {
			usageFields := map[string]any{"input_tokens": 100, "output_tokens": 20}
			if field != "" {
				usageFields[field] = 7
			}
			raw, _ := json.Marshal(usageFields)
			var u usage
			if err := json.Unmarshal(raw, &u); err != nil {
				t.Fatal(err)
			}
			round := newTurn(t.Context(), func() {}, llm.RequestOptions{}, &writer{w: io.Discard}, observed.NewResults(), &approver{}, t.TempDir(), "s")
			response := round.finish(&frame{Type: "result", Usage: &u})
			if response.Usage.OutputTokens != 20 || (field == "" && !slices.Contains(response.Usage.Unknown, llm.UsageReasoning)) || (field != "" && (response.Usage.ReasoningTokens != 7 || slices.Contains(response.Usage.Unknown, llm.UsageReasoning))) {
				t.Fatal(response.Usage)
			}

		})
	}
}

func TestResultUsageRetainsReasoningAcrossToolBatches(t *testing.T) {
	var records []llm.ActivityRecord
	opt := llm.RequestOptions{Identity: llm.RequestIdentity{TurnID: "turn"}, RecordActivity: func(_ context.Context, r llm.ActivityRecord) error { records = append(records, r); return nil }}
	round := newTurn(t.Context(), func() {}, opt, &writer{w: io.Discard}, observed.NewResults(), &approver{}, t.TempDir(), "s")
	round.usageReported = true
	reasoning := int64(7)
	result := &frame{Usage: &usage{InputTokens: 100, OutputTokens: 20, ReasoningTokens: &reasoning}}
	round.recordTurnUsage(result)
	if len(records) != 1 || records[0].Kind != "claude_usage" || records[0].ID != "turn" || records[0].Usage == nil || records[0].Usage.ReasoningTokens != 7 || slices.Contains(records[0].Usage.Unknown, llm.UsageReasoning) {
		t.Fatal(records)
	}
	// Stream usage and result-only reasoning must agree with generation stats.
	round.usageReported = false
	round.steps = stepStats{known: true, input: 100, output: 20, micros: 1000, reasoningUnknown: true}
	response := round.finish(result)
	if response.Generation == nil || response.Generation.Tokens != llm.ReportedTokens(response.Usage) || response.Usage.ReasoningTokens != 7 {
		t.Fatal(response.Generation, response.Usage)
	}
}

func TestNativeBashTimingRecordsActualHookBoundaries(t *testing.T) {
	now := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	var records []llm.ActivityRecord
	opt := llm.RequestOptions{RecordActivity: func(_ context.Context, r llm.ActivityRecord) error { records = append(records, r); return nil }}
	round := newTurn(t.Context(), func() {}, opt, &writer{w: io.Discard}, observed.NewResults(), &approver{}, t.TempDir(), "s")
	round.now = func() time.Time { return now }
	round.hook("gate", gateHook, hookInput{ToolName: "Bash", ToolUseID: "shell-1", ToolInput: []byte(`{"command":"cat example.txt"}`)})
	now = now.Add(3 * time.Second)
	round.handle(frame{Type: "user", Message: []byte(`{"content":[{"type":"tool_result","tool_use_id":"shell-1","content":"done"}]}`)})
	if len(records) != 2 || records[0].Kind != "native_bash" || !records[0].EndedAt.IsZero() || records[1].EndedAt.Sub(records[1].StartedAt) != 3*time.Second {
		t.Fatal(records)
	}
	round.endNativeBashCalls()
	if len(records) != 2 {
		t.Fatal("duplicate completion", records)
	}
}

func TestNativeAgentTranscriptAndMessageUsageAreLinkedAndDeduplicated(t *testing.T) {
	var activities []llm.ActivityRecord
	round := newTurn(t.Context(), func() {}, llm.RequestOptions{Identity: llm.RequestIdentity{TurnID: "turn"}, RecordActivity: func(_ context.Context, r llm.ActivityRecord) error { activities = append(activities, r); return nil }}, &writer{w: io.Discard}, observed.NewResults(), &approver{}, t.TempDir(), "session")
	parent := "agent-call"
	round.model = "opus-exact"
	messages := []frame{
		{Type: "assistant", Message: []byte(`{"id":"main","model":"opus-exact","content":[{"type":"text","text":"delegated"}],"usage":{"input_tokens":10,"output_tokens":2}}`)},
		{Type: "assistant", ParentToolUseID: &parent, Message: []byte(`{"id":"child","model":"haiku-exact","content":[{"type":"text","text":"child result"}],"usage":{"input_tokens":3,"cache_read_input_tokens":4,"output_tokens":5}}`)},
	}
	for _, f := range messages {
		round.handle(f)
	}
	round.handle(messages[1])
	read := int64(4)
	round.recordTurnUsage(&frame{Usage: &usage{InputTokens: 13, CacheReadInputTokens: &read, OutputTokens: 7}})
	totals := map[string]llm.Usage{}
	for _, r := range activities {
		if r.Kind == "provider_usage" {
			totals[r.Model] = *r.Usage
		}
	}
	if totals["opus-exact"].InputTokens != 10 || totals["haiku-exact"].InputTokens != 7 || totals["haiku-exact"].OutputTokens != 5 {
		t.Fatal("usage duplicated or native child lost", totals)
	}
	response := round.finish(&frame{})
	encoded, _ := json.Marshal(response)
	if !strings.Contains(string(encoded), "[Claude Code Agent agent-call]") || !strings.Contains(string(encoded), "child result") {
		t.Fatal("native child not linked in public transcript")
	}
}

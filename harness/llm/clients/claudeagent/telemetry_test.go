package claudeagent

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"slices"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/tool/observed"
)

func TestCompactRecordsStartEndAndCancellationCause(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	var activities []llm.ActivityRecord
	var records []llm.ConversationRecord
	client := &Client{contextTokens: map[string]int64{}}
	opt := llm.RequestOptions{
		RecordActivity: func(_ context.Context, r llm.ActivityRecord) error { activities = append(activities, r); return nil },
		RecordConversation: func(ctx context.Context, r llm.ConversationRecord) error {
			if ctx.Err() != nil {
				t.Fatal("canceled persistence")
			}
			records = append(records, r)
			return nil
		},
		Progress: func(p llm.Progress) {
			if p.Phase == "compacting" {
				cancel(errors.New("user_input"))
			}
		},
	}
	client.compact(ctx, llm.Request{}, opt, "s", 250000)
	if len(activities) != 1 || activities[0].Kind != "claude_compact" || activities[0].StartedAt.IsZero() {
		t.Fatal(activities)
	}
	if len(records) != 1 || !records[0].Interrupted || records[0].InterruptReason != "user_input" || records[0].StartedAt != activities[0].StartedAt || records[0].EndedAt.Before(records[0].StartedAt) || records[0].Usage != nil {
		t.Fatal(records)
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
			state := &compaction{}
			state.handle(frame{Type: "result", Usage: &u})
			if state.usage == nil || state.usage.OutputTokens != 20 || !slices.Equal(state.usage.Unknown, response.Usage.Unknown) {
				t.Fatal(state.usage)
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

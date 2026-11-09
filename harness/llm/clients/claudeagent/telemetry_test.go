package claudeagent

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/tool/observed"
)

func TestNormalTurnRecordsCompactionAndActualContext(t *testing.T) {
	for _, scenario := range []string{"auto_compact", "auto_compact_no_usage", "auto_compact_failed"} {
		t.Run(scenario, func(t *testing.T) {
			h := newHarness(t, scenario)
			var records []llm.ConversationRecord
			var activities []llm.ActivityRecord
			var phases []llm.Progress
			var logs bytes.Buffer
			old := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			defer slog.SetDefault(old)
			_, err := h.client.Respond(t.Context(), llm.Request{Input: []llm.Item{user("continue")}}, llm.RequestOptions{
				Identity: llm.RequestIdentity{SessionID: "compacting"}, Tools: h.tools,
				RecordConversation: func(_ context.Context, r llm.ConversationRecord) error { records = append(records, r); return nil },
				RecordActivity:     func(_ context.Context, r llm.ActivityRecord) error { activities = append(activities, r); return nil },
				Progress: func(p llm.Progress) {
					if p.Phase != "" || p.Reset {
						phases = append(phases, p)
					}
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(records) == 0 || records[0].Phase != "compact" || records[0].Provider != ProviderID {
				t.Fatal(records)
			}
			compact := records[0]
			if compact.StartedAt.IsZero() || compact.EndedAt.Before(compact.StartedAt) {
				t.Fatal(compact)
			}
			if len(phases) != 2 || phases[0].Phase != "compacting" || !phases[1].Reset {
				t.Fatal(phases)
			}
			var compacts []llm.ActivityRecord
			for _, r := range activities {
				if r.Kind == "claude_compact" {
					compacts = append(compacts, r)
				}
			}
			if len(compacts) != 2 || compacts[1].EndedAt.IsZero() {
				t.Fatal(compacts)
			}
			if scenario == "auto_compact_failed" {
				if compact.Compacted || compact.Error != "too_few_groups" || !strings.Contains(logs.String(), "too_few_groups") {
					t.Fatal(compact, logs.String())
				}
			} else if !compact.Compacted || compact.BeforeTokens != 200_000 || compact.AfterTokens != 1234 || compact.InputKnown || compact.InputTokens != 0 || compact.EndedAt.Sub(compact.StartedAt) != 1500*time.Millisecond {
				t.Fatal(compact)
			}
			got := h.client.lastContextTokens(sessionUUID("", "compacting"), llm.RequestOptions{})
			if scenario == "auto_compact_no_usage" {
				if len(records) != 1 || got != 0 {
					t.Fatal("invented actual input from post_tokens or aggregate result", records, got)
				}
			} else if last := records[len(records)-1]; !last.InputKnown || last.InputTokens != 2350 || got != 2350 {
				t.Fatal("context must be actual input including caches", records, got)
			}
		})
	}
}

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

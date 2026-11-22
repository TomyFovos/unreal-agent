package claudeagent

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/tool/observed"
)

func fakeUsageRecovery(f *fake, failed bool) int {
	step := func(parent *string, model string, input, cached, writes, output int, stop string) {
		event := func(value string) {
			f.emit(map[string]any{"type": "stream_event", "parent_tool_use_id": parent, "event": jsontext.Value(value)})
		}
		event(fmt.Sprintf(`{"type":"message_start","message":{"model":%q,"usage":{"input_tokens":%d,"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d}}}`, model, input, cached, writes))
		event(fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%q},"usage":{"output_tokens":%d}}`, stop, output))
		event(`{"type":"message_stop"}`)
	}
	step(nil, "claude-opus-5-5", 10, 100, 5, 128000, "max_tokens")
	parent := "native-child"
	step(&parent, "native-model", 3, 4, 0, 8192, "end_turn")
	step(nil, "claude-opus-5-5", 20, 150, 6, 512, "end_turn")
	// Assistant usage omits discarded thinking and reports only the last
	// fragment. The SDK's per-model map also includes earlier resumed runs.
	for _, output := range []int{1, 2} {
		f.emit(map[string]any{"type": "assistant", "message": jsontext.Value(fmt.Sprintf(`{"id":"main","model":"claude-opus-5-5","content":[{"type":"text","text":"done"}],"usage":{"output_tokens":%d}}`, output))})
	}
	f.emit(map[string]any{"type": "assistant", "parent_tool_use_id": parent, "message": jsontext.Value(`{"id":"child","model":"native-model","usage":{"output_tokens":2}}`)})
	f.emit(map[string]any{"type": "assistant", "message": jsontext.Value(`{"id":"error","model":"<synthetic>","usage":{"input_tokens":0,"output_tokens":0}}`)})
	subtype, stop := "success", "end_turn"
	if failed {
		subtype, stop = "error_during_execution", "max_output_tokens"
	}
	f.emit(map[string]any{
		"type": "result", "subtype": subtype, "is_error": failed, "stop_reason": stop,
		"usage":      jsontext.Value(`{"input_tokens":33,"cache_read_input_tokens":254,"cache_creation_input_tokens":11,"output_tokens":136704}`),
		"modelUsage": jsontext.Value(`{"claude-opus-5-5":{"inputTokens":999999,"outputTokens":999999}}`),
	})
	return 0
}

func TestNativeAgentUsageDeduplicatesStreamsAndKeepsMainScopeWithSameModel(t *testing.T) {
	var modelUsage llm.Usage
	opt := llm.RequestOptions{RecordActivity: func(_ context.Context, record llm.ActivityRecord) error {
		if record.Kind == "provider_usage" && record.Model == "claude-opus-5-5" {
			if modelUsage.InputTokens != 0 {
				t.Fatal("repeated model total", record)
			}
			modelUsage = *record.Usage
		}
		return nil
	}}
	round := newTurn(t.Context(), func() {}, opt, &writer{w: io.Discard}, observed.NewResults(), &approver{}, t.TempDir(), "session")
	round.model = "claude-opus-5-5"
	streams, stop := round.observeStreams(time.Hour)
	defer stop()
	round.streams = streams
	parent := "toolu_agent"
	for _, raw := range []string{
		`{"type":"message_start","message":{"id":"same-id","model":"claude-opus-5-5","usage":{"input_tokens":300}}}`,
		`{"type":"message_delta","usage":{"output_tokens":35}}`,
		`{"type":"message_stop"}`,
	} {
		f := frame{Type: "stream_event", ParentToolUseID: &parent, Event: jsontext.Value(raw)}
		streams.observe(f)
		round.handle(f)
	}
	for _, f := range []frame{
		{Type: "assistant", Message: []byte(`{"id":"same-id","model":"claude-opus-5-5","usage":{"input_tokens":100,"output_tokens":10}}`)},
		{Type: "assistant", ParentToolUseID: &parent, Message: []byte(`{"id":"same-id","model":"claude-opus-5-5","usage":{"output_tokens":3}}`)},
		{Type: "assistant", ParentToolUseID: &parent, Message: []byte(`{"id":"assistant-only","model":"claude-opus-5-5","usage":{"input_tokens":150}}`)},
		{Type: "assistant", ParentToolUseID: &parent, Message: []byte(`{"id":"assistant-only","model":"claude-opus-5-5","usage":{"output_tokens":15}}`)},
	} {
		round.handle(f)
	}
	result := &frame{Usage: &usage{InputTokens: 550, OutputTokens: 60}}
	round.recordTurnUsage(result)
	response := round.finish(result)
	if modelUsage.InputTokens != 550 || modelUsage.OutputTokens != 60 || response.Usage.InputTokens != 100 || response.Usage.OutputTokens != 10 ||
		round.lastAssistantInput == nil || *round.lastAssistantInput != 100 {
		t.Fatalf("model=%+v main=%+v main context=%v", modelUsage, response.Usage, round.lastAssistantInput)
	}
}

func TestExecutionUsageIncludesDiscardedOutputAndNativeModels(t *testing.T) {
	for _, failed := range []bool{false, true} {
		scenario := "usage_recovery"
		if failed {
			scenario += "_failure"
		}
		h := newHarness(t, scenario)
		for attempt := range 2 {
			var aggregate *llm.Usage
			models := map[string]llm.Usage{}
			_, err := h.client.Respond(t.Context(), llm.Request{Input: []llm.Item{user("work")}}, llm.RequestOptions{
				Identity: llm.RequestIdentity{SessionID: "usage-session", TurnID: fmt.Sprintf("turn-%d", attempt)}, Tools: h.tools,
				RecordActivity: func(_ context.Context, record llm.ActivityRecord) error {
					if record.Kind == "claude_usage" {
						if aggregate != nil || record.Model != "claude-opus-5-5" {
							t.Fatal("ambiguous execution usage", record)
						}
						aggregate = record.Usage
					}
					if record.Kind == "provider_usage" {
						models[record.Model] = *record.Usage
					}
					return nil
				},
			})
			if (err != nil) != failed {
				t.Fatal("changed failure handling", err)
			}
			if aggregate == nil || aggregate.InputTokens != 298 || aggregate.OutputTokens != 136704 || aggregate.CachedInputTokens != 254 || aggregate.CacheWriteInputTokens != 11 {
				t.Fatal("execution total lost output or includes previous runs", aggregate)
			}
			if len(models) != 2 || models["claude-opus-5-5"].InputTokens != 291 || models["claude-opus-5-5"].OutputTokens != 128512 || models["native-model"].InputTokens != 7 || models["native-model"].OutputTokens != 8192 {
				t.Fatal("bad model attribution or synthetic model included", models)
			}
		}
	}
}

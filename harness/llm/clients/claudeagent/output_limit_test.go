package claudeagent

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

func fakeOutputLimit(f *fake, scenario string) int {
	event := func(scope *string, value string) {
		f.emit(map[string]any{"type": "stream_event", "parent_tool_use_id": scope, "event": jsontext.Value(value)})
	}
	// A native Agent's limit must not inflate the main response's count.
	parent := "native-child"
	event(&parent, `{"type":"message_start","message":{"model":"child-model"}}`)
	event(&parent, `{"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":8000}}`)
	event(&parent, `{"type":"message_stop"}`)
	limit := 128000
	phases := []string{"thinking", "thinking", "thinking", "thinking"}
	switch scenario {
	case "output_limit_text":
		limit, phases = 4096, []string{"text"}
	case "output_limit_mixed":
		phases = []string{"thinking", "thinking", "text", ""}
	case "output_limit_unknown":
		phases = []string{"", "", "", ""}
	case "output_limit_no_stream":
		phases = nil
	}
	for _, phase := range phases {
		event(nil, `{"type":"message_start","message":{"model":"claude-opus-5-5"}}`)
		if phase != "" {
			event(nil, fmt.Sprintf(`{"type":"content_block_start","content_block":{"type":%q}}`, phase))
		}
		event(nil, fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":%d}}`, limit))
		event(nil, `{"type":"message_stop"}`)
	}
	if scenario == "output_limit_recovered" {
		f.result("Recovered.")
		return 0
	}
	message := fmt.Sprintf("API Error: Claude's response exceeded the %d output token maximum. To configure this behavior, set the CLAUDE_CODE_MAX_OUTPUT_TOKENS environment variable.", limit)
	stop := "max_output_tokens"
	if scenario == "output_limit_message_only" {
		stop = ""
	}
	if scenario == "output_limit_unrelated" {
		message, stop = "unrelated execution failure", ""
	}
	f.emit(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "stop_reason": stop, "errors": []string{message}})
	return 0
}

func TestOutputLimitFailureFakeCLI(t *testing.T) {
	t.Parallel()
	const advice = ". Its recovery attempts did not complete the response. Lowering effort or splitting the work among child agents may allow it to continue."
	for _, tc := range []struct{ scenario, explanation string }{
		{"thinking", "the output limit (128000 tokens) 4 times (thinking: 4)"},
		{"message_only", "the output limit (128000 tokens) 4 times (thinking: 4)"},
		{"text", "the output limit (4096 tokens) 1 time (text: 1)"},
		{"mixed", "the output limit (128000 tokens) 4 times (thinking: 2, text: 1, phase unknown: 1)"},
		{"unknown", "the output limit (128000 tokens) 4 times (phase unknown: 4)"},
		{"no_stream", "the output limit (128000 tokens) at least once (no stream count was available)"},
	} {
		t.Run(tc.scenario, func(t *testing.T) {
			t.Parallel()
			for _, record := range []bool{false, true} {
				h := newHarness(t, "output_limit_"+tc.scenario)
				opt := llm.RequestOptions{Tools: h.tools}
				if record {
					opt.RecordActivity = func(context.Context, llm.ActivityRecord) error { return nil }
				}
				_, err := h.client.Respond(t.Context(), llm.Request{Input: []llm.Item{user("work")}}, opt)
				failure, ok := err.(*Error)
				if !ok || failure.Code != "turn_failed" || failure.Message != "Claude Code reached "+tc.explanation+advice {
					t.Fatalf("record=%t: %v", record, err)
				}
				if strings.Contains(err.Error(), "CLAUDE_CODE_MAX_OUTPUT_TOKENS") {
					t.Fatal("misleading output-limit advice survived", err)
				}
			}
		})
	}
	for _, scenario := range []string{"recovered", "unrelated"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, "output_limit_"+scenario)
			_, err := h.client.Respond(t.Context(), llm.Request{Input: []llm.Item{user("work")}}, llm.RequestOptions{Tools: h.tools})
			if scenario == "recovered" && err != nil || scenario == "unrelated" && (err == nil || !strings.Contains(err.Error(), "unrelated execution failure")) {
				t.Fatal(scenario, err)
			}
		})
	}
}

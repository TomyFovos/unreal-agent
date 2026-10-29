package claudeagent

import (
	"context"
	"encoding/json/v2"
	"io"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/tool/observed"
)

func TestGenerationTimingExcludesToolAndApprovalWaits(t *testing.T) {
	now := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	var recorded []llm.Response
	opt := llm.RequestOptions{Tools: func(_ context.Context, response llm.Response) ([]llm.ToolOutcome, error) {
		recorded = append(recorded, response)
		now = now.Add(5 * time.Second) // recording/executing the tool batch
		return nil, nil
	}}
	round := newTurn(t.Context(), func() {}, opt, &writer{w: io.Discard}, observed.NewResults(), &approver{}, t.TempDir(), "session")
	round.now = func() time.Time { return now }
	event := func(value any) {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		round.handle(frame{Type: "stream_event", Event: data})
	}
	start := func(input int64) {
		event(map[string]any{"type": "message_start", "message": map[string]any{"usage": map[string]any{"input_tokens": input}}})
	}
	stop := func(output int64) {
		event(map[string]any{"type": "message_delta", "usage": map[string]any{"output_tokens": output}})
		event(map[string]any{"type": "message_stop"})
	}
	round.handle(frame{Type: "system", Subtype: "init", Model: "claude-test"})
	now = now.Add(10 * time.Millisecond) // first token wait is generation time
	start(100)
	now = now.Add(20 * time.Millisecond)
	stop(20)
	now = now.Add(7 * time.Second) // approval and Claude Code's tool execution
	round.batch(hookInput{ToolCalls: []batchCall{{ToolName: "Read", ToolUseID: "read-1", ToolInput: []byte(`{"file_path":"sample.txt"}`)}}})
	now = now.Add(15 * time.Millisecond)
	start(105)
	now = now.Add(25 * time.Millisecond)
	stop(7)
	now = now.Add(11 * time.Second) // CLI shutdown/result delivery is not generation
	final := round.finish(&frame{Type: "result", Result: "Done."})
	if len(recorded) != 1 {
		t.Fatalf("tool boundaries = %d, want 1", len(recorded))
	}
	for i, tc := range []struct {
		response llm.Response
		micros   int64
		input    int64
		output   int64
	}{{recorded[0], 30000, 100, 20}, {final, 40000, 105, 7}} {
		g := tc.response.Generation
		if g == nil || !g.Valid() || g.DurationMicros != tc.micros || g.Model != "claude-test" || g.Source != "client_clock" || g.Tokens != llm.ReportedTokens(tc.response.Usage) || g.Tokens.Input != tc.input || g.Tokens.Output != tc.output {
			t.Fatalf("boundary %d: generation statistics = %+v, want %d us for %d/%d tokens", i, g, tc.micros, tc.input, tc.output)
		}
	}
}

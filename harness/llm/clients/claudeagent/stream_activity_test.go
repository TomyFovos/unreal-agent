package claudeagent

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

func fakeStreamActivity(f *fake, scenario string) int {
	event := func(value string) {
		f.emit(map[string]any{"type": "stream_event", "parent_tool_use_id": nil, "event": jsontext.Value(value)})
	}
	retry := func(attempt int) {
		frame := map[string]any{"type": "system", "subtype": "api_retry", "attempt": attempt, "max_retries": 10, "retry_delay_ms": 5000, "error_status": 529, "error": "private-error-body"}
		if attempt == 1 {
			frame["error_status"] = nil
			frame["no_response"] = map[string]int64{"waited_ms": 300000, "retry_wait_ms": 2000}
		}
		f.emit(frame)
	}
	if scenario == "stream_activity_retry" {
		// Header/first-byte failures can occur before any message_start.
		retry(1)
	}
	event(`{"type":"message_start","message":{"model":"stream-model","usage":{"input_tokens":100,"cache_read_input_tokens":10,"cache_creation_input_tokens":5}}}`)
	delta := func() {
		event(`{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"秘密"}}`)
		event(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"body"}}`)
	}
	delta()
	if scenario == "stream_activity_retry" {
		retry(2)
		retry(3)
		for range 2 {
			f.emit(map[string]any{"type": "system", "subtype": "api_error", "error": map[string]any{"status": 503, "message": "private-error-body", "formatted": "private-formatted-error", "connection": map[string]any{"code": "ECONNRESET", "message": "private-connection-message"}}})
			event(`{"type":"error","error":{"type":"overloaded_error","message":"private-stream-error"}}`)
		}
	}
	// The test releases this fake only after observing several timer samples.
	// During a gap stdout is completely silent, so frame-driven sampling fails.
	released := false
	until := time.Now().Add(10 * time.Second)
	for time.Now().Before(until) {
		if _, err := os.Stat(os.Getenv("CLAUDEAGENT_ARGS") + ".release"); err == nil {
			released = true
			break
		}
		if scenario == "stream_activity_continuous" {
			delta()
		}
		time.Sleep(time.Millisecond)
	}
	if !released {
		return 2
	}
	event(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":20,"reasoning_tokens":7}}`)
	event(`{"type":"message_stop"}`)
	f.text("msg-final", "private-assistant-body")
	f.result("Done.")
	return 0
}

func TestStreamActivityFakeCLI(t *testing.T) {
	for _, scenario := range []string{"continuous", "gap", "retry"} {
		t.Run(scenario, func(t *testing.T) {
			h := newHarness(t, "stream_activity_"+scenario)
			h.client.activityInterval = 20 * time.Millisecond
			var mu sync.Mutex
			var records []llm.ActivityRecord
			var observations []streamObservation
			var callbackErr error
			progress, gaps, released := 0, 0, false
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			response, err := h.client.Respond(ctx, llm.Request{Input: []llm.Item{user("continue")}}, llm.RequestOptions{
				Identity: llm.RequestIdentity{SessionID: "stream-session", TurnID: "turn-stream"}, Tools: h.tools,
				RecordActivity: func(_ context.Context, record llm.ActivityRecord) error {
					if record.Kind != "claude_stream" {
						return nil
					}
					mu.Lock()
					defer mu.Unlock()
					var observation streamObservation
					if err := json.Unmarshal([]byte(record.Reason), &observation); err != nil {
						callbackErr = err
						return err
					}
					records = append(records, record)
					observations = append(observations, observation)
					if observation.Event == "progress" {
						progress++
					}
					if observation.Event == "no_delta" {
						gaps++
					}
					ready := scenario == "continuous" && progress >= 2 || scenario != "continuous" && progress >= 1 && gaps >= 2
					if ready && !released {
						released = true
						callbackErr = os.WriteFile(h.args+".release", nil, 0o600)
					}
					return callbackErr
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			if callbackErr != nil || !released {
				t.Fatal("timer failed to release fake CLI", callbackErr, observations)
			}
			var starts, stops, retries, apiErrors, streamErrors int
			var id string
			var lastDelta time.Time
			var previousCount int64
			for i, observation := range observations {
				activity := records[i]
				if activity.Provider != ProviderID || activity.Usage != nil {
					t.Fatal("observation entered accounting", activity)
				}
				switch observation.Event {
				case "message_start":
					starts++
					id = activity.ID
					if id != "turn-stream/stream/1" || activity.Model != "stream-model" || activity.StartedAt.IsZero() || !activity.EndedAt.IsZero() || observation.InputTokens == nil || *observation.InputTokens != 115 {
						t.Fatal(activity)
					}
					u := activity.RequestUsage
					if u == nil || u.Input == nil || *u.Input != 115 || u.Uncached == nil || *u.Uncached != 100 || u.Read == nil || *u.Read != 10 || u.Write == nil || *u.Write != 5 {
						t.Fatal("missing individual request cache counters", u)
					}
				case "progress", "message_stop":
					if activity.ID != id || observation.DeltaCount == nil || *observation.DeltaCount <= previousCount || observation.LastDeltaAt.IsZero() || observation.LastDeltaAt.Before(activity.StartedAt) || observation.LastDeltaAt.After(activity.EndedAt) || observation.ThinkingTokensEstimate == nil || observation.TextTokensEstimate == nil {
						t.Fatal("missing or stale stream counters", activity)
					}
					previousCount, lastDelta = *observation.DeltaCount, observation.LastDeltaAt
					if observation.Event == "message_stop" {
						stops++
						if observation.StopReason != "end_turn" || observation.OutputTokens == nil || *observation.OutputTokens != 20 || observation.ReasoningTokens == nil || *observation.ReasoningTokens != 7 {
							t.Fatal("completion usage or stop reason missing", activity)
						}
						if u := activity.RequestUsage; u == nil || u.Output == nil || *u.Output != 20 {
							t.Fatal("missing reported request output", u)
						}
						pairs := (*observation.DeltaCount - 1) / 2
						if *observation.DeltaCount != pairs*2+1 || *observation.ThinkingTokensEstimate != (pairs*6+3)/4 || *observation.TextTokensEstimate != pairs {
							t.Fatal("visible byte estimates are wrong", activity)
						}
					}
				case "no_delta":
					if activity.ID != id || lastDelta.IsZero() || activity.EndedAt.Before(lastDelta) || activity.Reason != `{"event":"no_delta"}` {
						t.Fatal("silent interval must contain only no_delta", activity)
					}
				case "api_retry":
					retries++
					count := retries - 1
					if retries == 1 {
						count = 1
						if activity.ID != "turn-stream" || !activity.StartedAt.IsZero() {
							t.Fatal("invented request before message_start", activity)
						}
					} else if activity.ID != id {
						t.Fatal("retry not linked to request", activity)
					}
					if observation.Count != int64(count) || observation.Attempt != retries || observation.MaxRetries != 10 || observation.RetryDelayMS != 5000 {
						t.Fatal("retry counters missing", activity)
					}
					if retries == 1 {
						if observation.ErrorStatus != nil || observation.NoResponse == nil || observation.NoResponse.WaitedMS != 300000 || observation.NoResponse.RetryWaitMS != 2000 {
							t.Fatal("first-byte retry details missing", activity)
						}
					} else if observation.ErrorStatus == nil || *observation.ErrorStatus != 529 || observation.NoResponse != nil {
						t.Fatal("HTTP retry status missing", activity)
					}
				case "api_error":
					apiErrors++
					if activity.ID != id || observation.Count != int64(apiErrors) || observation.ErrorStatus == nil || *observation.ErrorStatus != 503 {
						t.Fatal("API error counters missing", activity)
					}
				case "error":
					streamErrors++
					if activity.ID != id || observation.ErrorType != "overloaded_error" || observation.Count != int64(streamErrors) {
						t.Fatal("stream error counters missing", activity)
					}
				default:
					t.Fatal("unexpected stream observation", activity)
				}
			}
			if starts != 1 || stops != 1 || observations[len(observations)-1].Event != "message_stop" || scenario == "continuous" && progress < 2 || scenario != "continuous" && gaps < 2 {
				t.Fatal("stream lifecycle missing", observations)
			}
			if scenario == "retry" && (retries != 3 || apiErrors != 2 || streamErrors != 2) || scenario != "retry" && (retries+apiErrors+streamErrors != 0) {
				t.Fatal("retry/error frames missing or invented", observations)
			}
			encoded, _ := json.Marshal(records)
			if strings.Contains(string(encoded), "private") || strings.Contains(string(encoded), "body") || strings.Contains(string(encoded), "秘密") {
				t.Fatal("activity leaked contents")
			}
			if response.Usage.InputTokens != 115 || response.Usage.OutputTokens != 20 || response.Usage.ReasoningTokens != 7 || response.Generation == nil || response.Generation.Tokens.Output != 20 {
				t.Fatal("observation changed usage or TPS", response.Usage, response.Generation)
			}
		})
	}
}

func TestStreamActivityRequestIsolation(t *testing.T) {
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	var records []llm.ActivityRecord
	s := &streamActivities{turnID: "turn", active: map[string]*streamRequestActivity{}, counts: map[string]map[string]int64{}, now: func() time.Time { return now }, record: func(r llm.ActivityRecord) { records = append(records, r) }}
	emit := func(parent *string, event string) {
		t.Helper()
		s.observe(frame{Type: "stream_event", ParentToolUseID: parent, Event: []byte(event)})
	}
	last := func() streamObservation {
		t.Helper()
		var observation streamObservation
		if err := json.Unmarshal([]byte(records[len(records)-1].Reason), &observation); err != nil {
			t.Fatal(err)
		}
		return observation
	}
	// Unknown counters stay absent; deltas without a start do not invent one.
	emit(nil, `{"type":"content_block_delta","delta":{"type":"text_delta","text":"ignored"}}`)
	emit(nil, `{"type":`)
	if len(records) != 0 {
		t.Fatal(records)
	}
	emit(nil, `{"type":"message_start"}`)
	if last().InputTokens != nil {
		t.Fatal("invented input tokens", records)
	}
	parent := "agent-tool"
	emit(&parent, `{"type":"message_start"}`)
	emit(&parent, `{"type":"content_block_delta","delta":{"type":"text_delta","text":"child"}}`)
	now = now.Add(streamActivityInterval)
	s.progress()
	seen := map[string]streamObservation{}
	for _, r := range records[2:] {
		var observation streamObservation
		_ = json.Unmarshal([]byte(r.Reason), &observation)
		seen[r.ID] = observation
	}
	if seen["turn/stream/1"].Event != "no_delta" || seen["turn/stream/2"].Event != "progress" || seen["turn/stream/2"].ParentToolUseID != parent {
		t.Fatal("child traffic hid main stream silence", records)
	}
	// A retry can start a new stream without sending message_stop for the old.
	// Reset counters without fabricating a completion for the replaced stream.
	emit(nil, `{"type":"message_start"}`)
	emit(nil, `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":2}}`)
	emit(nil, `{"type":"message_stop"}`)
	observation := last()
	if records[len(records)-1].ID != "turn/stream/3" || observation.DeltaCount == nil || *observation.DeltaCount != 1 || observation.ThinkingTokensEstimate != nil || observation.TextTokensEstimate != nil || observation.StopReason != "tool_use" || observation.ReasoningTokens != nil {
		t.Fatal("stream counters leaked across requests", records)
	}
	emit(&parent, `{"type":"message_stop"}`)
	count := len(records)
	now = now.Add(streamActivityInterval)
	s.progress()
	if len(records) != count {
		t.Fatal("timer continued after message_stop", records)
	}
}

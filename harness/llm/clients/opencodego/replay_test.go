package opencodego

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

// The anonymized 2026-11-02 probe replays through the client: the same
// streams yield the outputs and usage the probe recorded, and the client sends
// every header the probe sent.
func TestProbeCaptureReplaysThroughClient(t *testing.T) {
	data, err := os.ReadFile("testdata/probe-2026-11-02.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture probeSummary
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatal(err)
	}
	if len(capture.Exchanges) != len(capture.Results) || len(capture.Exchanges) == 0 {
		t.Fatal("capture does not pair exchanges with results")
	}
	next := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchange := capture.Exchanges[next]
		next++
		for name := range exchange.RequestHeaders {
			if r.Header.Get(name) == "" {
				t.Errorf("exchange %d: header %s not sent", next, name)
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(exchange.Status)
		for _, event := range exchange.Events {
			payload, err := json.Marshal(event.Data)
			if err != nil {
				t.Error(err)
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Event, payload)
		}
	}))
	defer server.Close()
	client, err := NewClient(Config{APIKey: "go-key", BaseURL: testBaseURL(server)})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	for i, want := range capture.Results {
		response, err := client.Respond(t.Context(), llm.Request{Model: llm.Model{ID: probeModel, ReasoningEffort: want.Effort}}, llm.RequestOptions{CacheKey: "session"})
		if err != nil || response.Stop != llm.StopComplete {
			t.Fatal(i, want.Scenario, err)
		}
		var types []string
		for _, item := range response.Output {
			types = append(types, string(item.Type))
		}
		got := response.Usage
		if !slices.Equal(types, want.Output) || want.Usage == nil || got.InputTokens != want.Usage.InputTokens || got.CachedInputTokens != want.Usage.CachedInputTokens ||
			got.OutputTokens != want.Usage.OutputTokens || got.ReasoningTokens != want.Usage.ReasoningTokens || !bytes.Contains(got.Raw, []byte(`"billing_source":"go"`)) {
			t.Fatal(i, want.Scenario, types, got)
		}
		if want.Scenario != "tool_call" {
			continue
		}
		call, ok := response.Output[1].Data.(llm.ToolCall)
		reasoning, isReasoning := response.Output[0].Data.(llm.Reasoning)
		if !ok || call.Name != "lookup_number" || call.CallID == "" || call.Arguments != `{"key": "alpha"}` || !isReasoning || len(reasoning.Raw) == 0 {
			t.Fatal("tool call or resendable reasoning lost", response.Output)
		}
	}
	if next != len(capture.Exchanges) {
		t.Fatal("requests", next)
	}
}

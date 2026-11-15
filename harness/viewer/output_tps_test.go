package viewer

import (
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

// A child's row carries its latest response's measured output speed beside
// that response's usage, for the Orchestration cards.
func TestRowCarriesLatestOutputSpeed(t *testing.T) {
	timed := llm.Response{ID: "r1", Model: "m", Usage: llm.Usage{InputTokens: 1000, CachedInputTokens: 900, OutputTokens: 200},
		Generation: &llm.GenerationStatistics{Model: "m", Tokens: llm.TokenCounts{Output: 200, OutputKnown: true}, DurationMicros: 4_000_000, Source: "provider_api"}}
	untimed := llm.Response{ID: "r2", Model: "m", Usage: llm.Usage{InputTokens: 2000, CachedInputTokens: 1500, OutputTokens: 10}}
	m := New(Options{RecentLimit: 4})
	must(t, m.Replace("s", view("s", "g", 1,
		host.HistoryItem{Sequence: 1, Kind: sessionstore.ItemModelResponse, Data: sessionstore.ModelResponse{TurnID: "t", Response: timed}})))
	r := row(t, m, "s")
	if r.OutputTPS != 50 || r.LastUsage.Input != 1000 || r.LastUsage.CachedInput != 900 {
		t.Fatalf("speed=%v last=%+v", r.OutputTPS, r.LastUsage)
	}
	// A later response without timing keeps the last measured speed.
	second := host.HistoryItem{Sequence: 2, Kind: sessionstore.ItemModelResponse, Data: sessionstore.ModelResponse{TurnID: "t", Response: untimed}}
	must(t, m.Apply("s", host.Event{Generation: "g", Revision: 2, Kind: "item", Item: &second}))
	if r := row(t, m, "s"); r.OutputTPS != 50 || r.LastUsage.Input != 2000 {
		t.Fatalf("speed=%v last=%+v", r.OutputTPS, r.LastUsage)
	}
}

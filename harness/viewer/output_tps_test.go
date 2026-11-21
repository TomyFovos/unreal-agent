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

func TestRowDistinguishesMeasuredZeroFromMissingUsage(t *testing.T) {
	m := New(Options{})
	timed := llm.Response{Usage: llm.Usage{InputTokens: 100}, Generation: &llm.GenerationStatistics{
		Tokens: llm.TokenCounts{OutputKnown: true}, DurationMicros: 1_000_000, Source: "client_clock",
	}}
	first := host.HistoryItem{Sequence: 1, Kind: sessionstore.ItemModelResponse, Data: sessionstore.ModelResponse{Response: timed}}
	must(t, m.Replace("s", view("s", "g", 1, first)))
	r := row(t, m, "s")
	if !r.OutputTPSKnown || r.OutputTPS != 0 || CacheLabel(r.LastUsage) != "cache 0.00%" {
		t.Fatalf("measured zero was lost: %+v", r)
	}
	timed.Usage.Unknown = []llm.UsageField{llm.UsageCachedInput, llm.UsageOutput}
	timed.Generation = &llm.GenerationStatistics{DurationMicros: 1_000_000, Source: "client_clock"}
	second := host.HistoryItem{Sequence: 2, Kind: sessionstore.ItemModelResponse, Data: sessionstore.ModelResponse{Response: timed}}
	must(t, m.Apply("s", host.Event{Generation: "g", Revision: 2, Kind: "item", Item: &second}))
	r = row(t, m, "s")
	if r.OutputTPSKnown || CacheLabel(r.LastUsage) != "cache -" {
		t.Fatalf("missing counters were treated as measured: %+v", r)
	}
}

func TestRowKeepsResponseCacheRatioAcrossCompaction(t *testing.T) {
	m := New(Options{})
	compaction := host.HistoryItem{Sequence: 2, Kind: sessionstore.ItemHostRecord, Data: sessionstore.HostRecord{
		Version: 1, Kind: sessionstore.HostContext, Context: &sessionstore.ContextRecord{Usage: &llm.Usage{InputTokens: 10}},
	}}
	first := response(1, "response", 100, 10)
	r := first.Data.(sessionstore.ModelResponse)
	r.Response.Usage.CachedInputTokens = 80
	first.Data = r
	must(t, m.Replace("s", view("s", "g", 1, first, compaction)))
	got := row(t, m, "s")
	if got.Usage.Input != 110 || CacheLabel(got.LastUsage) != "cache 80.00%" {
		t.Fatalf("compaction changed response cache stats or was omitted from totals: %+v", got)
	}
}

package analysis

import (
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func TestAverageInputAndCanonicalPruningStatistics(t *testing.T) {
	a := New("session")
	for _, usage := range []llm.Usage{{InputTokens: 100}, {InputTokens: 300}, {}, {InputTokens: 999, Unknown: []llm.UsageField{llm.UsageInput}}} {
		a.Apply(host.HistoryItem{Data: sessionstore.ModelResponse{Response: llm.Response{Usage: usage}}})
	}
	for _, record := range []sessionstore.ContextRecord{
		{Kind: "summary", Usage: &llm.Usage{InputTokens: 200}},
		{Kind: "prune", Projections: []sessionstore.ContextProjection{{UnitID: "one"}, {UnitID: "two"}}, PrunedTokens: 15000},
		{Kind: "prune", UnitID: "legacy"},
		{Kind: "projection", UnitID: "bounded-at-recording"},
	} {
		a.Apply(host.HistoryItem{Data: sessionstore.HostRecord{Version: 1, Kind: sessionstore.HostContext, Context: &record}})
	}
	r := a.Snapshot(time.Now(), false, false, nil, nil)
	if r.AverageInputTokens != 200 || r.InputTokenResponses != 3 || r.Pruning != (Pruning{Results: 3, Epochs: 2, Tokens: 15000}) {
		t.Fatalf("wrong metrics: %+v", r)
	}
	for _, format := range []string{"json", "markdown"} {
		encoded, err := Encode(r, format)
		if err != nil {
			t.Fatal(err)
		}
		if format == "markdown" && (!strings.Contains(string(encoded), "200.0 (3 reported calls)") || !strings.Contains(string(encoded), "Pruned tool results  3")) {
			t.Fatal("missing metrics", string(encoded))
		}
	}
}

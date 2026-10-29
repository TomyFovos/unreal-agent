package contextengine

import (
	"fmt"
	"sync"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
)

func TestCompactionThresholdUsesWindowMaximumOutputAndFallback(t *testing.T) {
	c, err := (Config{}).Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if c.ToolResultKeepTokens != 15000 || c.ToolResultMinPruneTokens != 15000 {
		t.Fatal("wrong epoch defaults", c)
	}
	for _, test := range []struct{ window, output, want int64 }{{1000000, 100000, 800000}, {200000, 32000, 104000}, {0, 4096, min(c.FallbackWindow*8/10, c.FallbackWindow-4096-64000)}} {
		if got := c.Threshold(Runtime{ContextWindow: test.window}, test.output); got != test.want {
			t.Fatalf("threshold=%d want %d", got, test.want)
		}
	}
}

func TestOrderedIndexConcurrentMetadataAndImmutablePayload(t *testing.T) {
	e, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range 50 {
			e.ObserveOperation(operation.Operation{ID: operation.ID(fmt.Sprint(i)), Status: operation.StatusAwaiting})
			_ = e.State()
		}
	}()
	output := []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "original"}}
	id := e.Add(Unit{Kind: ToolResult, Class: Keep, Item: llm.Item{Type: llm.ItemToolResult, Data: llm.ToolResult{CallID: "call", Output: output}}}, "source")
	output[0].Value = "mutation"
	for range 50 {
		_ = e.Units()
	}
	wg.Wait()
	u, ok := e.ResolveUnit(id)
	if !ok || Text(u.Item) != "original" {
		t.Fatal("caller mutated index")
	}
	u.Item.Data.(llm.ToolResult).Output[0].Value = "second mutation"
	u, _ = e.ResolveUnit(id)
	if Text(u.Item) != "original" {
		t.Fatal("ResolveUnit aliases owned payload")
	}
}

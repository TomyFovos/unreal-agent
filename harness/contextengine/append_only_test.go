package contextengine

import (
	"fmt"
	"sync"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
)

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

package modelcatalog

import (
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"testing"
)

func TestConfiguredCatalogDoesNotInventModelsOrEfforts(t *testing.T) {
	models := []Model{{ID: "configured-a", Efforts: []llm.ReasoningEffort{"low", "medium"}}, {ID: "configured-b", Efforts: []llm.ReasoningEffort{"high"}}}
	c, err := Configured(models)
	if err != nil || !c.Available || len(c.Models) != 2 || len(c.Models[1].Efforts) != 1 {
		t.Fatal(c, err)
	}
	models[0].Efforts[0] = "max"
	if c.Models[0].Efforts[0] != "low" {
		t.Fatal("catalog retained mutable caller metadata")
	}
	for _, invalid := range [][]Model{
		{{ID: "a", Efforts: []llm.ReasoningEffort{"invented"}}},
		{{ID: "a", Efforts: []llm.ReasoningEffort{"low", "low"}}},
		{{ID: "a", Efforts: []llm.ReasoningEffort{"low"}, DefaultEffort: "high"}},
		{{ID: "a", Efforts: []llm.ReasoningEffort{"low"}}, {ID: "a", Efforts: []llm.ReasoningEffort{"high"}}},
		{{ID: "-model", Efforts: []llm.ReasoningEffort{"low"}}},
		{{ID: "a", Name: "\x1b[2J", Efforts: []llm.ReasoningEffort{"low"}}},
	} {
		if _, err = Configured(invalid); err == nil {
			t.Fatal("accepted invalid metadata")
		}
	}
	c, err = Configured(nil)
	if err != nil || c.Available || len(c.Models) != 0 || c.Problem != "catalog unavailable" {
		t.Fatal(c, err)
	}
}

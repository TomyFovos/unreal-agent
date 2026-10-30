package modelcatalog

import (
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"slices"
	"testing"
)

func TestConfiguredOmitsUltraAndResolvesDefault(t *testing.T) {
	for _, tc := range []struct {
		efforts, want []llm.ReasoningEffort
		defaultEffort llm.ReasoningEffort
	}{
		{[]llm.ReasoningEffort{"high", "ultra"}, []llm.ReasoningEffort{"high"}, "high"},
		{[]llm.ReasoningEffort{"high", "max", "ultra"}, []llm.ReasoningEffort{"high", "max"}, "max"},
	} {
		c, err := Configured([]Model{{ID: "legacy", Efforts: tc.efforts, DefaultEffort: "ultra"}})
		if err != nil || !c.Available || len(c.Models) != 1 {
			t.Fatal(c, err)
		}
		m := c.Models[0]
		if !slices.Equal(m.Efforts, tc.want) || m.DefaultEffort != tc.defaultEffort {
			t.Fatal(m)
		}
		if tc.efforts[len(tc.efforts)-1] != "ultra" {
			t.Fatal("modified operator configuration")
		}
	}
}

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

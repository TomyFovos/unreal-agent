package profile

import (
	"reflect"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/provider"
)

func selectedModel() provider.Selection {
	return provider.Selection{Version: 1, Provider: "openai", Model: provider.Model{ID: "configured-test-model", Family: "openai-reasoning"}}
}
func TestExplicitResolution(t *testing.T) {
	p := selectedModel()
	for _, s := range []Selection{Default(), {ID: "conservative", Version: 1}, {ID: "openai-reasoning", Version: 1, Source: "child request"}} {
		first, err := Resolve(s, p)
		if err != nil {
			t.Fatal(err)
		}
		second, err := Resolve(s, p)
		if err != nil || !reflect.DeepEqual(first, second) {
			t.Fatal("nondeterministic resolution")
		}

	}
	for _, s := range []Selection{{ID: "unknown", Version: 1, Source: "explicit"}, {ID: "conservative", Version: 2, Source: "explicit"}} {
		if _, err := Resolve(s, p); err == nil {
			t.Fatal("invalid profile accepted")
		}
	}
	s := Selection{ID: "openai-reasoning", Version: 1, Source: "child request"}
	p.Model.Family = "another"
	if _, err := Resolve(s, p); err == nil {
		t.Fatal("incompatible model accepted")
	}
}
func TestProfilesCannotEnableToolsOrAlterSchemas(t *testing.T) {
	schema := map[string]any{"type": "object", "required": []string{"revision"}}
	tools := []llm.Tool{{Name: "edit", Type: llm.ToolFunction, Description: "Edit", Parameters: schema}}
	p, err := Resolve(Selection{ID: "openai-reasoning", Version: 1, Source: "explicit"}, selectedModel())
	if err != nil {
		t.Fatal(err)
	}
	system, modified := p.Compose("Host lifecycle rules", tools)
	if system == "" || len(modified) != 1 || modified[0].Name != "edit" || modified[0].Type != tools[0].Type || !reflect.DeepEqual(modified[0].Parameters, schema) || tools[0].Description != "Edit" {
		t.Fatal("changed execution surface")
	}
	if modified[0].Description == tools[0].Description {
		t.Fatal("no guidance override")
	}
	_, none := p.Compose("", nil)
	if len(none) != 0 {
		t.Fatal("enabled new tool")
	}
}

func BenchmarkCompose(b *testing.B) {
	for _, s := range []Selection{Default(), {ID: "openai-reasoning", Version: 1, Source: "benchmark"}} {
		b.Run(s.ID, func(b *testing.B) {
			p, _ := Resolve(s, selectedModel())
			tools := []llm.Tool{{Name: "read"}, {Name: "edit"}, {Name: "write"}}
			for b.Loop() {
				p.Compose("Host rules", tools)
			}
		})
	}
}

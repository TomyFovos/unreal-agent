package profile

import (
	"context"
	"encoding/json/v2"
	"reflect"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/provider"
)

func selectedModel() provider.Selection {
	return provider.Selection{Version: 1, Provider: "openai", Model: provider.Model{ID: "configured-test-model", Family: "openai-reasoning"}}
}
func TestExplicitResolutionAndResume(t *testing.T) {
	p := selectedModel()
	for _, s := range []Selection{Default(), {ID: "openai-reasoning", Version: 1, Source: "child request"}} {
		first, err := Resolve(s, p)
		if err != nil {
			t.Fatal(err)
		}
		second, err := Resolve(s, p)
		if err != nil || !reflect.DeepEqual(first, second) {
			t.Fatal("nondeterministic resolution")
		}
		data, _ := json.Marshal(first.Selection())
		var restored Selection
		if err := json.Unmarshal(data, &restored); err != nil {
			t.Fatal(err)
		}
		if ValidateResume(restored, s) != nil {
			t.Fatal("resume failed")
		}
	}
	for _, s := range []Selection{{ID: "unknown", Version: 1, Source: "explicit"}, {ID: "conservative", Version: 2, Source: "explicit"}, {ID: "conservative", Version: 1}} {
		if _, err := Resolve(s, p); err == nil {
			t.Fatal("invalid profile accepted")
		}
	}
	s := Selection{ID: "openai-reasoning", Version: 1, Source: "child request"}
	p.Model.Family = "another"
	if _, err := Resolve(s, p); err == nil {
		t.Fatal("incompatible model accepted")
	}
	if ValidateResume(Default(), s) == nil {
		t.Fatal("resume silently changed profile")
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

type fixtureAdapter struct {
	t        *testing.T
	requests []llm.Request
}

func (a *fixtureAdapter) Respond(_ context.Context, r llm.Request, _ llm.RequestOptions) (llm.Response, error) {
	a.requests = append(a.requests, r)
	return llm.Response{Stop: llm.StopComplete, Output: []llm.Item{{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "fixture-read", Name: "read", Arguments: "{}"}}}, Usage: llm.Usage{InputTokens: 100, OutputTokens: 10}}, nil
}
func TestBehaviorComparisonKeepsFixtureAndModelFixed(t *testing.T) {
	a := &fixtureAdapter{t: t}
	base, _ := Resolve(Default(), selectedModel())
	override, _ := Resolve(Selection{ID: "openai-reasoning", Version: 1, Source: "benchmark"}, selectedModel())
	original := llm.Request{Model: llm.Model{ID: "configured-test-model", ReasoningEffort: llm.ReasoningEffortHigh}, Input: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "Read current revision before editing."}}}, Tools: []llm.Tool{{Name: "read", Type: llm.ToolFunction}}}
	results, err := Compare(t.Context(), a, []Resolved{base, override}, []Fixture{{Name: "read-before-edit", Request: original}})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].Usage.InputTokens != 100 || results[1].Usage.OutputTokens != 10 {
		t.Fatal("lost actual usage")
	}
	if !reflect.DeepEqual(a.requests[0].Model, a.requests[1].Model) || !reflect.DeepEqual(a.requests[0].Input[1:], a.requests[1].Input[1:]) {
		t.Fatal("comparison changed fixture settings")
	}
	if len(original.Input) != 1 || original.Tools[0].Description != "" {
		t.Fatal("mutated source fixture")
	}
	if results[0].Response.Output[0].Data.(llm.ToolCall).Name != results[1].Response.Output[0].Data.(llm.ToolCall).Name {
		t.Fatal("fixture behavior changed")
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

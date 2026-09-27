package ast

import (
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"testing"
)

type recorder struct{ specs []operation.Spec }

func (r *recorder) Submit(spec operation.Spec) operation.ID {
	r.specs = append(r.specs, spec)
	return "op"
}
func TestTranslatorIsPureAndUsesFixedSchemas(t *testing.T) {
	registry := tool.NewRegistry(Configure(tool.StaticTranslators{}), tool.ASTGrepName, tool.ASTEditName)
	if len(registry.StaticDefinitions()) != 2 {
		t.Fatal("definitions")
	}
	translator, _ := registry.Resolve(tool.ASTEditName)
	ctx := &recorder{}
	status := translator.Translate(ctx, llm.ToolCall{Arguments: `{"language":"go","query":"(identifier) @match","targets":[{"path":"/nonexistent"}],"replacement":"new"}`})
	if status.Error != "" || len(ctx.specs) != 1 {
		t.Fatal(status)
	}
}
func TestInvalidRequestsNeverDispatch(t *testing.T) {
	cases := []string{
		`{"language":"go","query":"(identifier) @match","targets":[{"path":"a"}],"replacement":"x","apply":true}`,
		`{"language":"go","query":"(identifier) @match","targets":[{"path":"a"}],"apply":false}`,
		`{"language":"rust","query":"x","targets":[{"path":"a"}],"replacement":"x"}`,
		`{"version":5,"language":"go","query":"x","targets":[{"path":"a"}],"replacement":"x"}`,
	}
	for _, args := range cases {
		ctx := &recorder{}
		got := (translator{edit: true}).Translate(ctx, llm.ToolCall{Arguments: args})
		if got.Error == "" || len(ctx.specs) != 0 {
			t.Fatal(got)
		}
	}
}

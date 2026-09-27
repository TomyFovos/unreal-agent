package native

import (
	"encoding/json/v2"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	files "github.com/unreallabsai/unreal-agent/harness/native"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"testing"
)

type recorder struct{ specs []operation.Spec }

func (r *recorder) Submit(s operation.Spec) operation.ID { r.specs = append(r.specs, s); return "op" }
func TestPureTranslationAndFixedSchemas(t *testing.T) {
	registry := tool.NewRegistry(Configure(tool.StaticTranslators{}), tool.ReadName, tool.WriteName, tool.EditName, tool.GrepName, tool.GlobName)
	if len(registry.StaticDefinitions()) != 5 {
		t.Fatal("missing definitions")
	}
	for action, args := range map[string]string{"read": `{"path":"/nonexistent","start_line":2}`, "write": `{"path":"/nonexistent","expected":{"exists":false},"content":"text"}`, "edit": `{"path":"/nonexistent","expected":{"exists":true,"digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"old_text":"old","new_text":"new"}`, "grep": `{"path":"/nonexistent","pattern":"text","glob":"**/*.go"}`, "glob": `{"path":"/nonexistent","glob":"**/*.go"}`} {
		translator, ok := registry.Resolve(action)
		if !ok {
			t.Fatal(action)
		}
		ctx := &recorder{}
		status := translator.Translate(ctx, llm.ToolCall{Arguments: args})
		if status.Error != "" || len(ctx.specs) != 1 {
			t.Fatal(action, status)
		}
		current := operation.Operation{ID: "op", Type: ctx.specs[0].Type, Version: ctx.specs[0].Version, MaxOutputLength: ctx.specs[0].MaxOutputLength, State: ctx.specs[0].State}
		state, err := operation.DecodeRemoteJobState(current)
		if err != nil {
			t.Fatal(err)
		}
		var request files.Request
		if json.Unmarshal(state.Plan.Data, &request) != nil || request.Action != action {
			t.Fatal(request)
		}
	}
}
func TestInvalidArgumentsNeverSubmit(t *testing.T) {
	for _, args := range []string{`{"path":"a","expected":{},"content":"x"}`, `{"path":"a","unknown":true}`, `{"path":"a","expected":{"exists":true},"content":"x"}`, `{"path":"a","content":"x"}`, `{"path":"a","expected":{"exists":false}}`} {
		ctx := &recorder{}
		status := New("write").Translate(ctx, llm.ToolCall{Arguments: args})
		if status.Error == "" || len(ctx.specs) != 0 {
			t.Fatal(args, status)
		}
	}
}

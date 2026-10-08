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

func TestTypedOutcomeClassifiesNoMatchWithoutChangingOperation(t *testing.T) {
	translator := New("edit").(tool.ResultFailureClassifier)
	spec, err := files.Spec(files.Request{Version: files.Version, Action: "read", Path: "fixture.txt"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		code   string
		failed bool
	}{{"ok", false}, {"applied", false}, {"binary", false}, {"no_match", true}, {"stale", true}, {"permission_denied", true}, {"unknown", true}} {
		t.Run(tc.code, func(t *testing.T) {
			op := operation.Operation{ID: "op", Type: spec.Type, Version: spec.Version, MaxOutputLength: spec.MaxOutputLength, Status: operation.StatusCompleted, State: spec.State}
			state, err := operation.DecodeRemoteJobState(op)
			if err != nil {
				t.Fatal(err)
			}
			state.Handle = files.EncodeResult(files.Result{Version: files.Version, Code: tc.code})
			state.TerminalResult = tc.code
			step, err := operation.UpdateRemoteJob(op, state, operation.StatusCompleted)
			if err != nil {
				t.Fatal(err)
			}
			before, _ := json.Marshal(step.Operation)
			if failed := translator.ResultFailed(tool.CallStatus{}, []operation.Operation{*step.Operation}); failed != tc.failed {
				t.Fatal("typed outcome mismatch", failed)
			}
			after, _ := json.Marshal(step.Operation)
			if string(before) != string(after) || step.Operation.Status != operation.StatusCompleted {
				t.Fatal("outcome classifier mutated canonical Operation")
			}
		})
	}
	if !translator.ResultFailed(tool.CallStatus{}, nil) {
		t.Fatal("missing typed receipt reported success")
	}
}

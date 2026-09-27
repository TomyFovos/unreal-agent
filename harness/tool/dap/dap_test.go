package dap

import (
	"encoding/json/v2"
	debug "github.com/unreallabsai/unreal-agent/harness/dap"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"testing"
)

type recorder struct{ specs []operation.Spec }

func (r *recorder) Submit(s operation.Spec) operation.ID {
	r.specs = append(r.specs, s)
	return "operation"
}
func TestTranslatorOnlyProducesTypedPlan(t *testing.T) {
	ctx := &recorder{}
	tr := New("owner")
	status := tr.Translate(ctx, llm.ToolCall{Arguments: `{"handle":{"id":"debug"},"command":"launch","start":{"adapter":"fake","program":"/does/not/exist","directory":"/does/not/exist"}}`})
	if status.Error != "" || len(ctx.specs) != 1 {
		t.Fatal("translation attempted I/O or failed", status)
	}
	spec := ctx.specs[0]
	state, err := operation.DecodeRemoteJobState(operation.Operation{Type: spec.Type, Version: spec.Version, State: spec.State, MaxOutputLength: spec.MaxOutputLength})
	if err != nil {
		t.Fatal(err)
	}
	var r debug.Request
	if json.Unmarshal(state.Plan.Data, &r) != nil || r.OwnerGeneration != "owner" || r.Version != 1 {
		t.Fatal("invalid plan")
	}
	for _, raw := range []string{`{"command":"secret","handle":{"id":"debug"}}`, `{"command":"threads","handle":{"id":"debug","generation":"g"},"owner_generation":"forged"}`} {
		if status := tr.Translate(ctx, llm.ToolCall{Arguments: raw}); status.Error == "" {
			t.Fatal("invalid request accepted")
		}
	}
	if len(ctx.specs) != 1 {
		t.Fatal("invalid request submitted")
	}
}

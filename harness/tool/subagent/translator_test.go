package subagent

import (
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"testing"
)

type ctx struct {
	eligible bool
	specs    []operation.Spec
}

func (c *ctx) Submit(s operation.Spec) operation.ID { c.specs = append(c.specs, s); return "op" }
func (c *ctx) CanFinish() bool                      { return c.eligible }
func TestFinishRejectsOtherPendingCallsBeforeSubmit(t *testing.T) {
	tr := translator{action: "finish", owner: "child"}
	call := llm.ToolCall{Name: "Finish", Arguments: `{"status":"completed","summary":"done","changedFiles":[],"tests":[],"blockers":[]}`}
	c := &ctx{}
	if s := tr.Translate(c, call); s.Error == "" || len(c.specs) != 0 {
		t.Fatal(s, c.specs)
	}
	c.eligible = true
	if s := tr.Translate(c, call); s.Error != "" || len(c.specs) != 1 {
		t.Fatal(s, c.specs)
	}
}

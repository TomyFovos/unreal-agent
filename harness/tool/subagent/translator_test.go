package subagent

import (
	"slices"
	"strings"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	runtime "github.com/unreallabsai/unreal-agent/harness/subagent"
	"testing"
)

func TestSendParentProgressIsOptional(t *testing.T) {
	extensions, err := Extensions("child", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, extension := range extensions {
		definition := extension.Definition.Tool
		if definition.Name != "SendParent" {
			continue
		}
		properties := definition.Parameters["properties"].(map[string]any)
		progress, ok := properties["progress"].(map[string]any)
		if !ok || progress["type"] != "boolean" || slices.Contains(definition.Parameters["required"].([]string), "progress") {
			t.Fatal("progress must be an optional boolean", definition.Parameters)
		}
		if description, _ := progress["description"].(string); !strings.Contains(description, "does not wake") || !strings.Contains(description, "False (default)") {
			t.Fatal("missing progress delivery guidance", progress)
		}
		for _, tc := range []struct {
			name, arguments string
			progress        bool
		}{
			{"default", `{"text":"report"}`, false},
			{"question", `{"text":"report","progress":false}`, false},
			{"progress", `{"text":"report","progress":true}`, true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				c := &ctx{}
				if status := extension.Translator.Translate(c, llm.ToolCall{Name: "SendParent", Arguments: tc.arguments}); status.Error != "" || len(c.specs) != 1 {
					t.Fatal(status, c.specs)
				}
				spec := c.specs[0]
				plan, err := runtime.DecodePlan(operation.Operation{ID: "send", Type: spec.Type, Version: spec.Version, State: spec.State, MaxOutputLength: spec.MaxOutputLength})
				if err != nil || plan.Action != "parent" || plan.ParentID != "child" || plan.Text != "report" || plan.Progress != tc.progress {
					t.Fatal(plan, err)
				}
			})
		}
		return
	}
	t.Fatal("SendParent definition missing")
}

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

func TestApproveAnswersAChildRequest(t *testing.T) {
	tr := translator{action: "approve", owner: "parent"}
	c := &ctx{}
	if s := tr.Translate(c, llm.ToolCall{Name: "SubagentApprove", Arguments: `{"handle":"child-op","request":"req-1","allow":true}`}); s.Error != "" || len(c.specs) != 1 {
		t.Fatal(s, c.specs)
	}
	p, err := runtime.DecodePlan(operation.Operation{ID: "approve", Type: c.specs[0].Type, Version: c.specs[0].Version, MaxOutputLength: operation.DefaultMaxOutputLength, State: c.specs[0].State})
	if err != nil || p.Action != "approve" || p.Handle != "child-op" || p.Request != "req-1" || !p.Allow {
		t.Fatal(p, err)
	}
	if s := tr.Translate(c, llm.ToolCall{Name: "SubagentApprove", Arguments: `{"handle":"child-op","allow":true}`}); s.Error == "" {
		t.Fatal("approval without a request")
	}
}

// A child with worker templates is a parent too; one without is not.
func TestChildWithWorkersGetsParentTools(t *testing.T) {
	enabled := func(templates map[string]runtime.Template, child bool) map[string]bool {
		extensions, err := Extensions("s", templates, child)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, e := range extensions {
			out[e.Definition.Tool.Name] = e.Enabled
		}
		return out
	}
	worker := map[string]runtime.Template{"worker": {}}
	if got := enabled(nil, true); got["SubagentStart"] || got["SubagentApprove"] || !got["Finish"] || !got["SendParent"] {
		t.Fatal("plain child", got)
	}
	if got := enabled(worker, true); !got["SubagentStart"] || !got["SubagentApprove"] || !got["Finish"] {
		t.Fatal("lead child", got)
	}
	if got := enabled(worker, false); !got["SubagentStart"] || !got["SubagentApprove"] || got["Finish"] {
		t.Fatal("parent", got)
	}
}

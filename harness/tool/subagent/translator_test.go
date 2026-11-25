package subagent

import (
	"context"
	"encoding/json/v2"
	"slices"
	"strings"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	runtime "github.com/unreallabsai/unreal-agent/harness/subagent"
	"testing"
)

func TestSubagentStartRuntimeArgumentFollowsOwner(t *testing.T) {
	for _, child := range []bool{false, true} {
		extensions, err := Extensions("owner", map[string]runtime.Template{"worker": {Workspace: t.TempDir(), Runtime: []byte(`{}`)}}, child)
		if err != nil {
			t.Fatal(err)
		}
		var requested *runtime.RuntimeRequest
		BindRuntime(extensions, t.Context(), nil, func(_ context.Context, _ *host.Session, _ string, template runtime.Template, selection *runtime.RuntimeRequest) (runtime.Template, error) {
			requested = selection
			return template, nil
		})
		for _, extension := range extensions {
			if extension.Definition.Tool.Name != "SubagentStart" {
				continue
			}
			_, advertised := extension.Definition.Tool.Parameters["properties"].(map[string]any)["runtime"]
			c := &ctx{}
			result := extension.Translator.Translate(c, llm.ToolCall{Name: "SubagentStart", Arguments: `{"template":"worker","task":"check","runtime":{"provider":"openai","model":"gpt-6-luna","effort":"medium"}}`})
			if result.Error != "" || len(c.specs) != 1 || advertised == child || (requested != nil) == child {
				t.Fatalf("child=%t: advertised=%t, requested=%+v, status=%+v", child, advertised, requested, result)
			}
			for _, tc := range []struct {
				args string
				want runtime.RuntimeRequest
			}{
				{`{"template":"worker","model":"deepseek-v4.1-flash","effort":"max"}`, runtime.RuntimeRequest{Model: "deepseek-v4.1-flash", Effort: "max"}},
				{`{"template":"worker","provider":"opencode-go","model":"deepseek-v4.1-flash"}`, runtime.RuntimeRequest{Provider: "opencode-go", Model: "deepseek-v4.1-flash"}},
			} {
				props := extension.Definition.Tool.Parameters["properties"].(map[string]any)
				for _, name := range []string{"provider", "model", "effort"} {
					if _, ok := props[name]; !ok || slices.Contains(extension.Definition.Tool.Parameters["required"].([]string), name) {
						t.Fatalf("child=%t: missing optional %s argument", child, name)
					}
				}
				c := &ctx{}
				result := extension.Translator.Translate(c, llm.ToolCall{Name: "SubagentStart", Arguments: tc.args})
				if result.Error != "" || len(c.specs) != 1 || requested == nil || *requested != tc.want {
					t.Fatalf("child=%t: requested=%+v, want %+v; status=%+v", child, requested, tc.want, result)
				}
			}
		}
	}
}

func TestSubagentStartKeepsOptionalBriefAndPreviewsChildText(t *testing.T) {
	template := runtime.Template{Workspace: t.TempDir(), Runtime: []byte(`{}`)}
	ext, err := Extensions("parent", map[string]runtime.Template{"worker": template}, false)
	if err != nil {
		t.Fatal(err)
	}
	var start *translator
	for _, item := range ext {
		if item.Definition.Tool.Name == "SubagentStart" {
			if want := []string{"template"}; !slices.Equal(item.Definition.Tool.Parameters["required"].([]string), want) {
				t.Fatal("task and brief fields must be optional", item.Definition.Tool.Parameters)
			}
			start = item.Translator.(*translator)
		}
	}
	if start == nil {
		t.Fatal("no start translator")
	}
	for _, tc := range []struct {
		task  string
		brief runtime.BriefFields
	}{
		{"legacy task", runtime.BriefFields{}},
		{"", runtime.BriefFields{}},
		{"", runtime.BriefFields{Title: "Structured task", Purpose: "Review", Inputs: "source", Outputs: "review",
			Verification: "verify", AcceptanceCriteria: "check"}},
	} {
		args, _ := json.Marshal(map[string]any{"template": "worker", "task": tc.task, "brief": tc.brief})
		c := &ctx{}
		result := start.Translate(c, llm.ToolCall{Name: "SubagentStart", Arguments: string(args)})
		if result.Error != "" || len(c.specs) != 1 {
			t.Fatal("start refused optional fields", result)
		}
		spec := c.specs[0]
		op := operation.Operation{ID: "start", Type: spec.Type, Version: spec.Version, State: spec.State, MaxOutputLength: spec.MaxOutputLength, Status: operation.StatusReady}
		plan, err := runtime.DecodePlan(op)
		if err != nil || plan.Text != tc.task || plan.Brief != tc.brief {
			t.Fatal("start changed structured or free text", plan, err)
		}
		brief := &runtime.DelegationBrief{Root: template.Workspace, Fields: tc.brief}
		handle := runtime.Handle{Version: 1, ParentID: plan.ParentID, ChildID: plan.ChildID, OperationID: op.ID, Brief: brief}
		state, _ := operation.DecodeRemoteJobState(op)
		state.Handle, _ = json.Marshal(handle)
		step, err := operation.UpdateRemoteJob(op, state, operation.StatusCompleted)
		if err != nil {
			t.Fatal(err)
		}
		output, err := start.TranslateResult("call", result, []operation.Operation{*step.Operation})
		if err != nil {
			t.Fatal(err)
		}
		var preview struct{ Preview string }
		child := runtime.ChildConfig{ParentID: handle.ParentID, ChildID: handle.ChildID, OperationID: handle.OperationID, Task: tc.task, Brief: brief}
		peer, err := child.InitialInput().DecodePeerMessage()
		if err != nil || json.Unmarshal([]byte(output.Output[0].Value), &preview) != nil || preview.Preview != peer.Text {
			t.Fatal("tool preview differs from child text", preview, peer, err)
		}
	}
}

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
	waiting  []string
	specs    []operation.Spec
}

func (c *ctx) Submit(s operation.Spec) operation.ID { c.specs = append(c.specs, s); return "op" }
func (c *ctx) CanFinish() bool                      { return c.eligible }
func (c *ctx) FinishWaitingFor() []string           { return c.waiting }
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

func TestFinishErrorsExplainFieldsAndWaitTargets(t *testing.T) {
	tr := translator{action: "finish", owner: "child"}
	for _, tc := range []struct{ args, want string }{
		{`{"answers":"synthetic-private"}`, "unknown field /answers"},
		{`{"status":42}`, "field /status"},
		{`{"changedFiles":[42]}`, "field /changedFiles/0"},
		{`{"summary":{"token":"synthetic-private"}}`, "field /summary"},
		{`{"status":"done","summary":"text"}`, "status must be completed or failed"},
		{`{"status":"completed","summary":"  "}`, "summary must contain text"},
		{`{"summary":`, "malformed JSON"},
	} {
		c := &ctx{eligible: true}
		got := tr.Translate(c, llm.ToolCall{Name: "Finish", Arguments: tc.args})
		if !strings.Contains(got.Error, tc.want) || strings.Contains(got.Error, "synthetic-private") || len(c.specs) != 0 {
			t.Fatalf("%s: %+v", tc.want, got)
		}
	}
	c := &ctx{waiting: []string{"call read-1 (read)", "operation op-1 (Bash, awaiting)"}}
	got := tr.Translate(c, llm.ToolCall{Name: "Finish", Arguments: `{"status":"completed","summary":"done"}`})
	for _, want := range append(c.waiting, "Wait for their results", "call Finish alone") {
		if !strings.Contains(got.Error, want) {
			t.Fatal(got.Error)
		}
	}
	if len(c.specs) != 0 {
		t.Fatal("ineligible Finish submitted")
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
	if got := enabled(nil, true); got["SubagentStart"] || got["SubagentApprove"] || got["SubagentReview"] || !got["Finish"] || !got["SendParent"] {
		t.Fatal("plain child", got)
	}
	if got := enabled(worker, true); !got["SubagentStart"] || !got["SubagentApprove"] || !got["SubagentReview"] || !got["Finish"] {
		t.Fatal("lead child", got)
	}
	if got := enabled(worker, false); !got["SubagentStart"] || !got["SubagentApprove"] || !got["SubagentReview"] || got["Finish"] {
		t.Fatal("parent", got)
	}
}

func TestReviewIsOptionalMetadataWithOptionalReason(t *testing.T) {
	extensions, err := Extensions("lead", map[string]runtime.Template{"worker": {}}, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range extensions {
		if e.Definition.Tool.Name != "SubagentReview" {
			continue
		}
		if slices.Contains(e.Definition.Tool.Parameters["required"].([]string), "reason") {
			t.Fatal("reason is required")
		}
		for _, args := range []string{`{"handle":"child","decision":"accept"}`, `{"handle":"child","decision":"reject","reason":"test failed"}`} {
			c := &ctx{}
			status := e.Translator.Translate(c, llm.ToolCall{Name: "SubagentReview", Arguments: args})
			if status.Error != "" || len(c.specs) != 1 {
				t.Fatal(status)
			}
			spec := c.specs[0]
			plan, err := runtime.DecodePlan(operation.Operation{ID: "review", Type: spec.Type, Version: spec.Version, State: spec.State, MaxOutputLength: spec.MaxOutputLength})
			if err != nil || plan.Action != "review" || plan.ParentID != "lead" || plan.Handle != "child" || plan.Review == "" {
				t.Fatal(plan, err)
			}
		}
		c := &ctx{}
		if status := e.Translator.Translate(c, llm.ToolCall{Name: "SubagentReview", Arguments: `{"handle":"child","decision":"maybe"}`}); status.Error == "" || len(c.specs) != 0 {
			t.Fatal("invalid decision submitted", status)
		}
		return
	}
	t.Fatal("SubagentReview missing")
}

func TestSubagentToolUsageReceiptAndInspectionActions(t *testing.T) {
	template := runtime.Template{Workspace: t.TempDir(), Runtime: []byte(`{"Provider":{"provider":"opencode-go","model":{"id":"deepseek"}},"ReasoningEffort":"max"}`)}
	var tr = &translator{action: "start", owner: "parent", templates: map[string]runtime.Template{"worker": template}}
	makeOp := func(args string) operation.Operation {
		t.Helper()
		c := &ctx{}
		status := tr.Translate(c, llm.ToolCall{Name: "SubagentStart", Arguments: args})
		if status.Error != "" || len(c.specs) != 1 {
			t.Fatalf("tool %s: %+v", args, status)
		}
		spec := c.specs[0]
		return operation.Operation{ID: "handle", ToolName: "SubagentStart", Type: spec.Type, Version: spec.Version, State: spec.State, MaxOutputLength: spec.MaxOutputLength, Status: operation.StatusAwaiting}
	}
	op := makeOp(`{"template":"worker","task":"plan","brief":{"budget_steps":10}}`)
	plan, err := runtime.DecodePlan(op)
	if err != nil || plan.Brief.BudgetSteps != 10 || !plan.UsageReceipt {
		t.Fatal(plan, err)
	}
	state, err := operation.DecodeRemoteJobState(op)
	if err != nil {
		t.Fatal(err)
	}
	state.Handle, _ = json.Marshal(runtime.Handle{Version: 1, ParentID: plan.ParentID, ChildID: plan.ChildID, OperationID: op.ID,
		QueuedAhead: new(2), UsageAtStart: []string{"opencode-go · 12 calls · charge $0.12"}})
	step, err := operation.UpdateRemoteJob(op, state, operation.StatusAwaiting)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := tr.AcceptedResult("call", *step.Operation)
	if err != nil || receipt == nil {
		t.Fatal(receipt, err)
	}
	text := receipt.Output[0].Value
	for _, expected := range []string{`"Status":"queued"`, `"Ahead":2`, `"Selection":"opencode-go / deepseek / max"`, `"Budget":"10"`, "12 calls"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("missing %s from %s", expected, text)
		}
	}
	for _, action := range []string{"list", "show", "wait"} {
		op := makeOp(`{"template":"*","action":"` + action + `","handle":"owned","all":true,"timeout_seconds":1}`)
		p, err := runtime.DecodePlan(op)
		if err != nil || p.Action != action || p.Handle != "owned" || p.WaitSeconds != 1 || !p.InspectAll {
			t.Fatalf("inspection plan %+v: %v", p, err)
		}
	}
}

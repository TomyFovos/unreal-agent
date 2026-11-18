package subagent_test

import (
	"context"
	"encoding/json/v2"
	"os"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	sub "github.com/unreallabsai/unreal-agent/harness/subagent"
)

func TestChildStartRecordsResolvedRuntimeInParent(t *testing.T) {
	dir := t.TempDir()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	runtime := []byte(`{"Provider":{"provider":"fake","model":{"id":"test-model"}},"ReasoningEffort":"high"}`)
	templates := map[string]sub.Template{"worker-test": {Layer: "lead", Workspace: dir, Runtime: runtime, Policy: permission.Config{Tools: []string{"Finish", "SendParent"}}}}
	config := sub.Config{Directory: dir, Binary: binary, Arguments: []string{"-test.run=^TestChildServerHelper$"}, Environment: append(os.Environ(), "UNREAL_SUBAGENT_HELPER=1"), Templates: templates}
	h, err := host.New(t.Context(), host.Config{Directory: dir, Build: factory(config, adapter(func(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
		return llm.Response{}, nil
	}))})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	parent, err := h.Create(t.Context(), host.Options{ID: "parent", Policy: permission.Unrestricted()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = sub.StartChild(t.Context(), parent, parent.Generation, "start-record", "worker-test", "finish", templates, nil, nil); err != nil {
		t.Fatal(err)
	}
	op := awaitChild(t, parent, true)
	plan, err := sub.DecodePlan(op)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Configuration.Layer != "lead" {
		t.Fatal("canonical start lost its resolved layer", plan.Configuration.Layer)
	}
	child, err := sub.ReadChild(t.Context(), dir, plan.ChildID, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if child.Configuration == nil || child.Configuration.Template != "worker-test" || child.Configuration.Layer != "lead" {
		t.Fatal("child creation lost its template or resolved layer", child.Configuration)
	}
	v, err := parent.Inspect(0, 256)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, item := range v.History.Items {
		if record, ok := item.Data.(sessionstore.HostRecord); ok && record.Activity != nil && record.Activity.Kind == "child_start" {
			count++
			r := record.Activity
			if r.Template != "worker-test" || r.Layer != "lead" || r.Provider != "fake" || r.Model != "test-model" || r.Effort != "high" || r.ID != string(op.ID) || r.StartedAt.IsZero() {
				data, _ := json.Marshal(r)
				t.Fatal(string(data))
			}
		}
	}
	if count != 1 {
		t.Fatalf("child starts=%d", count)
	}
	spec, err := sub.NewSpec(sub.Plan{Version: 1, Action: "review", ParentID: parent.ID, Handle: op.ID, Review: "reject"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parent.SubmitOperation(t.Context(), parent.Generation, "review-command", "review-op", "SubagentReview", spec); err != nil {
		t.Fatal(err)
	}
	reviewOp := awaitInstructionChild(t, parent, "review-op")
	if reviewOp.Status != operation.StatusCompleted {
		t.Fatal("parent review operation failed", reviewOp)
	}
	v, err = parent.Inspect(0, 256)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range v.History.Items {
		if record, ok := item.Data.(sessionstore.HostRecord); ok && record.ChildReview != nil && record.ChildReview.Handle == string(op.ID) {
			if record.ChildReview.Decision != "reject" || record.ChildReview.Reason != "" {
				t.Fatal("review lost optional reason", record)
			}
			return
		}
	}
	t.Fatal("tool path did not append parent review")
}

package subagent_test

import (
	"context"
	"encoding/json/v2"
	"os"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
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
	templates := map[string]sub.Template{"worker-test": {Workspace: dir, Runtime: runtime, Policy: permission.Config{Tools: []string{"Finish", "SendParent"}}}}
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
	v, err := parent.Inspect(0, 256)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, item := range v.History.Items {
		if record, ok := item.Data.(sessionstore.HostRecord); ok && record.Activity != nil && record.Activity.Kind == "child_start" {
			count++
			r := record.Activity
			if r.Template != "worker-test" || r.Model != "test-model" || r.Effort != "high" || r.ID != string(op.ID) || r.StartedAt.IsZero() {
				data, _ := json.Marshal(r)
				t.Fatal(string(data))
			}
		}
	}
	if count != 1 {
		t.Fatalf("child starts=%d", count)
	}
}

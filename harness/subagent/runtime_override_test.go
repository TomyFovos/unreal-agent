package subagent_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	sub "github.com/unreallabsai/unreal-agent/harness/subagent"
)

func TestChildRuntimeOverrides(t *testing.T) {
	for _, tc := range []struct {
		name      string
		request   *sub.RuntimeRequest
		model     string
		effort    llm.ReasoningEffort
		errorCode string
	}{
		{name: "template", model: "template-model", effort: "high"},
		{name: "model", request: &sub.RuntimeRequest{Model: "review-model"}, model: "review-model", effort: "high"},
		{name: "effort", request: &sub.RuntimeRequest{Effort: "low"}, model: "template-model", effort: "low"},
		{name: "both", request: &sub.RuntimeRequest{Model: "review-model", Effort: "max"}, model: "review-model", effort: "max"},
		{name: "legacy effort", request: &sub.RuntimeRequest{Effort: "ultra"}, model: "template-model", effort: "max"},
		{name: "invalid effort", request: &sub.RuntimeRequest{Effort: "invented"}, errorCode: "invalid_effort"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			template := sub.Template{Workspace: dir,
				Runtime:  []byte(`{"Provider":{"provider":"fake","model":{"id":"template-model","family":"test-family"},"endpoint":"https://template.invalid"},"ReasoningEffort":"high","SystemPrompt":"template prompt","FutureSetting":{"value":true}}`),
				Policy:   permission.Config{Tools: []string{"Finish", "SendParent"}},
				Baseline: &permission.Config{ReadRoots: []string{dir}},
				Default:  &sub.RuntimeRequest{Provider: "fake", Model: "template-model", Effort: "high"},
			}
			templates := map[string]sub.Template{"worker": template}
			original, err := json.Marshal(template)
			if err != nil {
				t.Fatal(err)
			}
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
			// Explicit overrides do not inherit or resolve a different provider.
			resolve := sub.RuntimeResolver(nil)
			if tc.request != nil {
				resolve = func(context.Context, *host.Session, sub.Template, *sub.RuntimeRequest) (sub.Template, error) {
					t.Error("template override invoked the parent's runtime resolver")
					return template, errors.New("unexpected resolver")
				}
			}
			receipt, err := sub.StartChild(t.Context(), parent, parent.Generation, "start", "worker", "finish", templates, tc.request, resolve)
			if tc.errorCode != "" {
				var failure *sub.RuntimeError
				if !errors.As(err, &failure) || failure.Code != tc.errorCode {
					t.Fatalf("start error = %v; want %s", err, tc.errorCode)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			op := awaitChild(t, parent, true)
			if op.Status != operation.StatusCompleted {
				t.Fatal("child failed", op)
			}
			plan, err := sub.DecodePlan(op)
			if err != nil {
				t.Fatal(err)
			}
			if s := sessionstore.SelectionFromConfiguration(plan.Configuration.Runtime); s == nil || s.Provider != "fake" || s.Model != tc.model || s.Effort != tc.effort {
				t.Fatal("plan runtime", s)
			}
			// All remaining runtime and template fields must stay identical.
			var before, after map[string]any
			if err = json.Unmarshal(template.Runtime, &before); err != nil {
				t.Fatal(err)
			}
			if err = json.Unmarshal(plan.Configuration.Runtime, &after); err != nil {
				t.Fatal(err)
			}
			after["Provider"].(map[string]any)["model"].(map[string]any)["id"] = "template-model"
			after["ReasoningEffort"] = "high"
			if !reflect.DeepEqual(before, after) {
				t.Fatal("override changed other runtime fields", after)
			}
			chosen := *plan.Configuration
			chosen.Runtime = template.Runtime
			chosenJSON, err := json.Marshal(chosen)
			if err != nil || string(original) != string(chosenJSON) {
				t.Fatal("override changed template capabilities or defaults", chosen)
			}
			child, err := sub.ReadChild(t.Context(), dir, plan.ChildID, 0, 256)
			if err != nil || child.Configuration == nil {
				t.Fatal("child configuration", child, err)
			}
			if s := sessionstore.SelectionFromConfiguration(child.Configuration.Runtime); s == nil || s.Model != tc.model || s.Effort != tc.effort {
				t.Fatal("recorded child runtime", s)
			}
			view, err := parent.Inspect(0, 256)
			if err != nil {
				t.Fatal(err)
			}
			starts := 0
			for _, item := range view.History.Items {
				if record, ok := item.Data.(sessionstore.HostRecord); ok && record.Activity != nil && record.Activity.Kind == "child_start" {
					starts++
					if record.Activity.Model != tc.model || record.Activity.Effort != string(tc.effort) {
						t.Fatal("recorded startup runtime", record.Activity)
					}
				}
			}
			if starts != 1 {
				t.Fatalf("child_start records = %d; want 1", starts)
			}
			retried, err := sub.StartChild(t.Context(), parent, parent.Generation, "start", "worker", "finish", templates, tc.request, resolve)
			if err != nil || retried != receipt {
				t.Fatal("same-input retry", retried, receipt, err)
			}
			changed := &sub.RuntimeRequest{Model: "different-model"}
			if tc.request != nil {
				changed = nil
			}
			if _, err = sub.StartChild(t.Context(), parent, parent.Generation, "start", "worker", "finish", templates, changed, resolve); !errors.Is(err, host.ErrConflict) {
				t.Fatal("same-input retry accepted different overrides", err)
			}
			unchanged, err := json.Marshal(templates["worker"])
			if err != nil || string(original) != string(unchanged) {
				t.Fatal("override mutated the registered template", err)
			}
		})
	}
}

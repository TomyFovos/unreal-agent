package host

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

func TestTextOnlyChildRecoveryCompletesCanonicalResponseWithoutInference(t *testing.T) {
	for _, test := range []struct {
		name   string
		stop   llm.StopReason
		status string
	}{
		{"complete", llm.StopComplete, "completed"},
		{"refused", llm.StopRefused, "failed"},
		{"output limit", llm.StopMaxOutputTokens, "failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			f := func(ctx context.Context, _ session.ID) (Runtime, error) {
				selection := sessionstore.RuntimeSelection{Version: 1, Provider: "claude-code", Model: "synthetic", Effort: "medium"}
				b := contextbuilder.NewBuilder()
				b.(interface {
					ConfigureRuntime(llm.Model, string, []llm.Tool, []tool.Skill, bool, bool)
				}).ConfigureRuntime(llm.Model{ID: selection.Model, ReasoningEffort: selection.Effort}, "", nil, nil, true, true)
				return Runtime{Selection: &selection, Builder: b, Tools: tool.NewRegistry(tool.StaticTranslators{}), Operations: operation.NewLocalOperationManager(ctx), LLM: modelFunc(func(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
					calls.Add(1)
					return llm.Response{Model: "synthetic-observed", Stop: test.stop, Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "canonical text"}}}}, nil
				})}, nil
			}
			dir := t.TempDir()
			h, err := New(t.Context(), Config{Directory: dir, Build: f})
			if err != nil {
				t.Fatal(err)
			}
			defer h.Close()
			o := Options{ID: "text-child", Lifecycle: "child", Configuration: []byte(`{"Provider":{"provider":"claude-code","model":{"id":"synthetic"}},"ReasoningEffort":"medium"}`)}
			s, err := h.Create(t.Context(), o)
			if err != nil {
				t.Fatal(err)
			}
			selectionSubmit(t, s, "child-task")
			var turn session.TurnID
			waitSelection(t, func() bool {
				view, _ := s.Inspect(0, 128)
				for _, item := range view.History.Items {
					if response, ok := item.Data.(sessionstore.ModelResponse); ok {
						turn = response.TurnID
						return true
					}
				}
				return false
			})
			// Exercise the durable state after response and before completion,
			// without asking the model again or fabricating a Finish tool.
			stop(t, s)
			if s.Finish() != nil || calls.Load() != 1 {
				t.Fatal("unexpected pre-recovery state")
			}
			if err = h.Close(); err != nil {
				t.Fatal(err)
			}
			h, err = New(t.Context(), Config{Directory: dir, Build: f})
			if err != nil {
				t.Fatal(err)
			}
			defer h.Close()
			o.TextOnlyChild = true
			s, err = h.Resume(t.Context(), o)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.Wait(timeout(t)); err != nil {
				t.Fatal(err)
			}
			finish := s.Finish()
			if finish == nil || finish.ModelTurnID != turn || finish.OperationID != "" || finish.Result.Status != test.status || finish.Result.Summary != "canonical text" || calls.Load() != 1 {
				t.Fatal("text child recovery changed response or repeated inference")
			}
			view, _ := s.Inspect(0, 128)
			if len(view.Operations) != 0 {
				t.Fatal("text completion created a Tool Operation")
			}
			n := 0
			for _, item := range view.History.Items {
				if record, ok := item.Data.(sessionstore.HostRecord); ok && record.Kind == "finish" {
					n++
				}
			}
			if n != 1 {
				t.Fatal("duplicated canonical completion")
			}
		})
	}
}

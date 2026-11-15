package subagent

import (
	"context"
	"encoding/json/v2"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"github.com/unreallabsai/unreal-agent/harness/tool/bash"
	"github.com/unreallabsai/unreal-agent/harness/tool/observed"
)

type endReportAdapter func(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error)

func (f endReportAdapter) Respond(ctx context.Context, request llm.Request, opt llm.RequestOptions) (llm.Response, error) {
	return f(ctx, request, opt)
}
func (endReportAdapter) RequiresFullToolBatch() bool { return true }

func TestCompletedInputSuppressesReadShowAndWaitReports(t *testing.T) {
	for _, transport := range []string{"claude_code", "shell"} {
		for _, action := range []string{"show", "wait", "unread", "running_show", "other_handle_prefix"} {
			t.Run(transport+"/"+action, func(t *testing.T) {
				const handle operation.ID = "child-one"
				report := "== child-one  worker  completed\nresult: completed\nsummary: final report\n"
				saw := true
				switch action {
				case "wait":
					report = "== child-two  worker  awaiting\n" + report
				case "unread":
					report, saw = "== child-two  worker  completed\nresult: completed\nsummary: other child\n", false
				case "running_show":
					report, saw = "== child-one  worker  awaiting\ntask: still working\n", false
				case "other_handle_prefix":
					report, saw = "== child-one-other  worker  completed\nresult: completed\n", false
				}
				results := observed.NewResults()
				read, release := make(chan struct{}), make(chan struct{})
				var calls atomic.Int32
				model := endReportAdapter(func(ctx context.Context, _ llm.Request, opt llm.RequestOptions) (llm.Response, error) {
					if calls.Add(1) != 1 {
						return llm.Response{}, nil
					}
					call := llm.ToolCall{CallID: "read-report", Name: tool.ClaudeCodeName, Arguments: `{"tool":"Bash","input":{"command":"unreal-agent subagent ` + action + ` child-one"}}`}
					if transport == "shell" {
						args, _ := json.Marshal(map[string]string{"command": "printf '%s' '" + report + "'"})
						call.Name, call.Arguments = tool.BashName, string(args)
					} else {
						results.Put(call.CallID, observed.Result{Content: report})
					}
					_, err := opt.Tools(ctx, llm.Response{Output: []llm.Item{{Type: llm.ItemToolCall, Data: call}}})
					if err != nil {
						return llm.Response{}, err
					}
					close(read)
					select {
					case <-release:
					case <-ctx.Done():
						return llm.Response{}, ctx.Err()
					}
					return llm.Response{}, nil
				})
				workspace := t.TempDir()
				h, err := host.New(t.Context(), host.Config{Directory: t.TempDir(), Build: func(ctx context.Context, _ session.ID) (host.Runtime, error) {
					return host.Runtime{Builder: contextbuilder.NewBuilder(), LLM: model,
						Tools:      tool.NewRegistry(tool.StaticTranslators{ClaudeCode: observed.New(results), Bash: bash.New(bash.Config{Shell: "/bin/sh", Directory: workspace, BaseDirectory: workspace})}, tool.ClaudeCodeName, tool.BashName),
						Operations: operation.NewLocalOperationManager(ctx)}, nil
				}})
				if err != nil {
					t.Fatal(err)
				}
				defer h.Close()
				s, err := h.Create(t.Context(), host.Options{ID: "parent", Policy: permission.Unrestricted()})
				if err != nil {
					t.Fatal(err)
				}
				data, _ := json.Marshal("read the child report")
				if _, err = s.Submit(t.Context(), s.Generation, inbox.Input{ID: "read", Kind: inbox.InputExternal, Payload: data}); err != nil {
					t.Fatal(err)
				}
				select {
				case <-read:
				case <-time.After(5 * time.Second):
					t.Fatal("parent did not read report")
				}
				m, err := NewManager(t.Context(), Config{Owner: s})
				if err != nil {
					t.Fatal(err)
				}
				defer m.Close()
				if got := m.parentSaw(handle); got != saw {
					view, _ := s.Inspect(0, 256)
					t.Fatalf("parentSaw=%v, want %v; operations=%+v; history=%+v", got, saw, view.Operations, view.History.Items)
				}
				spec, err := NewSpec(Plan{Version: 1, Action: "start", ParentID: s.ID, Template: "worker", Configuration: &Template{Workspace: workspace, Runtime: []byte(`{}`)}, Text: "task", Notify: true})
				if err != nil {
					t.Fatal(err)
				}
				op := operation.Operation{ID: handle, Type: spec.Type, Version: spec.Version, State: spec.State, MaxOutputLength: spec.MaxOutputLength, Status: operation.StatusReady}
				state, _ := operation.DecodeRemoteJobState(op)
				step, err := operation.UpdateRemoteJob(op, state, operation.StatusCompleted)
				if err != nil {
					t.Fatal(err)
				}
				op = *step.Operation
				close(release)
				// The owner invokes this after the turn ends. The callback
				// retains the existing canonical show/wait deduplication.
				input, err := m.CompletedInput(op)
				if err != nil {
					t.Fatal(err)
				}
				if (input == nil) != saw {
					t.Fatalf("completion notice=%+v, already read=%v", input, saw)
				}
				if input != nil {
					peer, err := input.DecodePeerMessage()
					if err != nil || peer.Kind != "ended" || peer.Handle != string(handle) || !strings.Contains(peer.Text, "ended: completed") {
						t.Fatal(peer, err)
					}
				}
			})
		}
	}
}

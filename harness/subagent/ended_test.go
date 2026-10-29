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

type endedAdapter struct{ entered, release chan struct{} }

func (a *endedAdapter) Respond(ctx context.Context, _ llm.Request, _ llm.RequestOptions) (llm.Response, error) {
	select {
	case <-a.entered:
	default:
		close(a.entered)
		select {
		case <-a.release:
		case <-ctx.Done():
			return llm.Response{}, ctx.Err()
		}
	}
	return llm.Response{}, nil
}

type endReportAdapter func(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error)

func (f endReportAdapter) Respond(ctx context.Context, request llm.Request, opt llm.RequestOptions) (llm.Response, error) {
	return f(ctx, request, opt)
}
func (endReportAdapter) RequiresFullToolBatch() bool { return true }

func TestNotifyEndedSuppressesReadShowAndWaitReports(t *testing.T) {
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
				m.notifyEnded(Plan{ParentID: s.ID}, handle, operation.StatusCompleted, operation.RemoteJobState{})
				close(release)
				m.workers.Wait()
				view, _ := s.Inspect(0, 256)
				count := 0
				for _, item := range view.History.Items {
					if input, ok := item.Data.(inbox.Input); ok && input.Kind == inbox.InputPeer {
						peer, _ := input.DecodePeerMessage()
						if peer.Kind == "ended" {
							count++
						}
					}
				}
				want := 1
				if saw {
					want = 0
				}
				if count != want {
					t.Fatalf("ended notices=%d, want %d", count, want)
				}
			})
		}
	}
}

func TestNotifyEndedBatchesChildrenWhileParentBusy(t *testing.T) {
	model := &endedAdapter{entered: make(chan struct{}), release: make(chan struct{})}
	h, err := host.New(t.Context(), host.Config{Directory: t.TempDir(), Build: func(ctx context.Context, _ session.ID) (host.Runtime, error) {
		return host.Runtime{Builder: contextbuilder.NewBuilder(), LLM: model, Tools: tool.NewRegistry(tool.StaticTranslators{}), Operations: operation.NewLocalOperationManager(ctx)}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	s, err := h.Create(t.Context(), host.Options{ID: "parent", Policy: permission.Unrestricted()})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal("working")
	if _, err = s.Submit(t.Context(), s.Generation, inbox.Input{ID: "work", Kind: inbox.InputExternal, Payload: data}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-model.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("parent did not start")
	}
	m, err := NewManager(t.Context(), Config{Owner: s})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	for _, id := range []operation.ID{"child-one", "child-two"} {
		m.notifyEnded(Plan{ParentID: s.ID}, id, operation.StatusCompleted, operation.RemoteJobState{TerminalError: "detail " + string(id)})
	}
	close(model.release)
	deadline := time.After(5 * time.Second)
	for {
		view, err := s.Inspect(0, 256)
		if err != nil {
			t.Fatal(err)
		}
		var messages []inbox.PeerMessage
		for _, item := range view.History.Items {
			if input, ok := item.Data.(inbox.Input); ok && input.Kind == inbox.InputPeer {
				peer, _ := input.DecodePeerMessage()
				if peer.Kind == "ended" {
					messages = append(messages, peer)
				}
			}
		}
		if len(messages) > 0 {
			if len(messages) != 1 || !strings.Contains(messages[0].Text, "Child child-one ended: completed") || !strings.Contains(messages[0].Text, "Child child-two ended: completed") || !strings.Contains(messages[0].Text, "detail child-two") {
				t.Fatalf("ended messages = %+v", messages)
			}
			// Join the sender before checking that no second notification followed.
			m.Close()
			view, _ = s.Inspect(0, 256)
			count := 0
			for _, item := range view.History.Items {
				if input, ok := item.Data.(inbox.Input); ok && input.Kind == inbox.InputPeer {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("peer messages = %d, want 1", count)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("no completion notification")
		case <-time.After(time.Millisecond):
		}
	}
}

package subagent_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudeagent"
	"github.com/unreallabsai/unreal-agent/harness/llm/responsesapi"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/provider"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	sub "github.com/unreallabsai/unreal-agent/harness/subagent"
)

func TestParentModelFailureKeepsChildAndReceivesReport(t *testing.T) {
	providerFailure := func(code, stage, reason string, status int, retryable bool) error {
		return &provider.Error{Code: code, Status: status, Diagnostic: &responsesapi.FailureDiagnostic{
			Stage: stage, Status: status, Reason: reason, Retryable: retryable,
		}}
	}
	for _, tc := range []struct {
		name     string
		cause    error
		response *llm.Failure
		wait     bool
	}{
		{name: "usage_reset", wait: true},
		{name: "claude_session_limit", cause: &claudeagent.Error{Code: "turn_failed", Message: "You've hit your limit · resets 1:40am"}},
		{name: "rate_limit_without_reset", cause: providerFailure("rate_limited", "http_status", "rate_limit_exceeded", 429, true)},
		{name: "network", cause: providerFailure("request_failed", "transport", "connection_error", 0, true)},
		{name: "server_error", cause: providerFailure("remote_failure", "http_status", "server_error", 503, true)},
		{name: "overload", cause: providerFailure("remote_failure", "http_status", "server_is_overloaded", 529, true)},
		{name: "context_overflow", cause: providerFailure("invalid_request", "http_status", "context_length_exceeded", 400, false)},
		{name: "context_overflow_response", response: &llm.Failure{Code: "context_length_exceeded", Message: "context window exceeded"}},
		{name: "authentication", cause: providerFailure("unauthenticated", "http_status", "invalid_token", 401, false)},
		{name: "invalid_request", cause: providerFailure("invalid_request", "http_status", "invalid_request_error", 400, false)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			requests := make(chan llm.Request, 12)
			var calls atomic.Int32
			model := adapter(func(ctx context.Context, r llm.Request, _ llm.RequestOptions) (llm.Response, error) {
				if calls.Add(1) == 1 {
					if tc.wait {
						return llm.Response{}, &responsesapi.APIError{StatusCode: 429, Type: "usage_limit_reached", ResetAt: time.Now().Add(3 * time.Second)}
					}
					return llm.Response{Failure: tc.response}, tc.cause
				}
				select {
				case requests <- r:
				case <-ctx.Done():
					return llm.Response{}, ctx.Err()
				}
				return llm.Response{Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "report received"}}}}, nil
			})
			h, parent, child := startFailureTestParent(t, dir, model)
			submitFailureTestInput(t, parent, "trigger-failure", "work that will fail")
			view := awaitParentModelFailure(t, parent)
			if !view.Running {
				t.Fatalf("model failure closed parent: %s, operations: %+v", view.Failure, view.Operations)
			}
			if len(view.Operations) != 1 || view.Operations[0].ID != child.ID || view.Operations[0].Status != operation.StatusAwaiting {
				t.Fatal("failure changed the live child Operation", view.Operations)
			}
			if tc.wait && (view.Progress == nil || view.Progress.Mode != "usage_limit_wait" || !view.Progress.RetryAt.After(time.Now())) {
				t.Fatal("usage reset wait lost", view.Progress)
			}
			if !tc.wait && view.Progress != nil {
				t.Fatal("failed model call still has live progress", view.Progress)
			}
			awaitBoundary(t, func() bool {
				state, err := parent.ContextState(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if tc.wait {
					wait := state.Waiting.Current["usage_limit_wait:model"]
					return len(wait.Sources) > 0 && wait.RetryAt.Equal(view.Progress.RetryAt)
				}
				wait := state.Waiting.Current["model_failure:parent"]
				return len(wait.Sources) > 0 && wait.Text != ""
			})
			attached, err := h.Attach(parent.ID)
			if err != nil || attached != parent {
				t.Fatal("reattaching replaced the live owner", attached, err)
			}
			if got := calls.Load(); got != 1 || len(endedNotices(t, attached, child.ID)) != 0 {
				t.Fatal("failure retried or ended the child before its report", got)
			}
			if err := os.WriteFile(filepath.Join(dir, "release-child"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			result := awaitChild(t, attached, true)
			handle, err := sub.DecodeHandle(result)
			if err != nil || result.Status != operation.StatusCompleted || handle.Finish == nil || handle.Finish.Result.Summary != "child complete" {
				t.Fatal("original child did not complete", result, handle, err)
			}
			awaitBoundary(t, func() bool { return len(endedNotices(t, attached, child.ID)) > 0 })
			ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
			defer cancel()
			select {
			case request := <-requests:
				reports := 0
				for _, item := range request.Input {
					if message, ok := item.Data.(llm.Message); ok {
						if peer, err := modelPeer(message.Text); err == nil && peer.Kind == "ended" && peer.Handle == string(child.ID) && strings.Contains(peer.Text, "child complete") {
							reports++
						}
					}
				}
				if reports != 1 {
					t.Fatal("expected one report in the waking request", reports, request.Input)
				}
			case <-ctx.Done():
				t.Fatal("parent was not woken by the child report or existing reset", ctx.Err())
			}
			if _, err = attached.Stop(ctx, attached.Generation, inbox.StopWhenIdle, "test complete"); err != nil {
				t.Fatal(err)
			}
			if err = attached.Wait(ctx); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 2 || len(endedNotices(t, attached, child.ID)) != 1 {
				t.Fatal("child report woke the model or was persisted more than once", calls.Load())
			}
			if tc.name == "claude_session_limit" {
				reopened, err := h.Resume(ctx, host.Options{ID: parent.ID, Policy: permission.Unrestricted()})
				if err != nil {
					t.Fatal(err)
				}
				if _, err = reopened.Stop(ctx, reopened.Generation, inbox.StopWhenIdle, "check replay"); err != nil {
					t.Fatal(err)
				}
				if err = reopened.Wait(ctx); err != nil {
					t.Fatal(err)
				}
				if calls.Load() != 2 || len(endedNotices(t, reopened, child.ID)) != 1 {
					t.Fatal("reopen replayed the report or a model request", calls.Load())
				}
			}
		})
	}
}

func TestStopsAfterParentModelFailure(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		name := "human_stop"
		if shutdown {
			name = "host_shutdown"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			h, parent, child := startFailureTestParent(t, dir, adapter(func(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
				return llm.Response{}, errors.New("parent model unavailable")
			}))
			plan, err := sub.DecodePlan(child)
			if err != nil {
				t.Fatal(err)
			}
			submitFailureTestInput(t, parent, "fail", "try the model")
			if v := awaitParentModelFailure(t, parent); !v.Running {
				t.Fatal("parent stopped before a stop was requested", v.Failure)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
			defer cancel()
			if shutdown {
				if err = h.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err = parent.Stop(ctx, parent.Generation, inbox.StopHard, "human requested session stop"); err != nil {
					t.Fatal(err)
				}
				if err = parent.Wait(ctx); err != nil {
					t.Fatal(err)
				}
			}
			result := awaitChild(t, parent, true)
			if result.ID != child.ID || result.Status != operation.StatusCanceled {
				t.Fatal("stop did not cancel the original child", result)
			}
			snapshot, err := sub.ReadChildSnapshot(ctx, dir, plan.ChildID)
			if err != nil || snapshot.Finish != nil {
				t.Fatal("stop yielded a child Finish", snapshot.Finish, err)
			}
			if shutdown {
				_, reopened, _ := boundaryParent(t, dir, host.Resume)
				awaitBoundary(t, func() bool { return len(endedNotices(t, reopened, child.ID)) > 0 })
				notices := endedNotices(t, reopened, child.ID)
				snapshot, err = sub.ReadChildSnapshot(ctx, dir, plan.ChildID)
				if err != nil || snapshot.End == nil || snapshot.End.EndedBy != "host_restart" || len(notices) != 1 {
					t.Fatal("shutdown outcome was not reconciled once", snapshot.End, notices, err)
				}
			} else if snapshot.End != nil {
				t.Fatal("human cancellation was classified as host restart", snapshot.End)
			}
		})
	}
}

func startFailureTestParent(t *testing.T, dir string, model llm.Adapter) (*host.Host, *host.Session, operation.Operation) {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	templates := map[string]sub.Template{"worker": {Workspace: dir, Runtime: []byte(`{}`), Policy: permission.Config{Tools: []string{"Finish"}}}}
	cfg := sub.Config{Directory: dir, Binary: binary, Arguments: []string{"-test.run=^TestChildServerHelper$"}, Environment: append(os.Environ(), "UNREAL_SUBAGENT_HELPER=1"), Templates: templates}
	h, err := host.New(t.Context(), host.Config{Directory: dir, Build: factory(cfg, model)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	parent, err := h.Create(t.Context(), host.Options{ID: "parent", Policy: permission.Unrestricted()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = sub.StartChild(t.Context(), parent, parent.Generation, "start", "worker", "finish-after-release", templates, nil, nil); err != nil {
		t.Fatal(err)
	}
	child := waitReady(t, parent)
	awaitBoundary(t, func() bool {
		_, err := os.Stat(filepath.Join(dir, "child-running"))
		return err == nil
	})
	return h, parent, child
}

func submitFailureTestInput(t *testing.T, parent *host.Session, id inbox.ID, text string) {
	t.Helper()
	payload, _ := json.Marshal(text)
	if _, err := parent.Submit(t.Context(), parent.Generation, inbox.Input{ID: id, Kind: inbox.InputExternal, Payload: payload}); err != nil {
		t.Fatal(err)
	}
}

func awaitParentModelFailure(t *testing.T, parent *host.Session) (v host.View) {
	t.Helper()
	awaitBoundary(t, func() bool {
		var err error
		v, err = parent.Inspect(0, 256)
		if err != nil {
			t.Fatal(err)
		}
		if !v.Running || v.Progress != nil && v.Progress.Mode == "usage_limit_wait" {
			return true
		}
		for _, item := range v.History.Items {
			switch data := item.Data.(type) {
			case sessionstore.HostRecord:
				if data.Activity != nil && data.Activity.Kind == llm.ActivityModelFailure && data.Activity.Reason != "" {
					return true
				}
			case sessionstore.ModelResponse:
				if data.Response.Failure != nil {
					return true
				}
			}
		}
		return false
	})
	return v
}

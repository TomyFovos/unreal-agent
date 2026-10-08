//go:build linux || darwin

package provider

import (
	"context"
	"errors"
	"syscall"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudecode"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

func TestClaudeAdapterCloseCancelsAndDrainsOwnedProcess(t *testing.T) {
	f := testclaude.New(t)
	f.Set(t, testclaude.Config{Wait: true, Child: true})
	m := Model{ID: "configured-model", Capabilities: []string{"reasoning"}}
	r, err := New(Defaults(map[string][]Model{"claude-code": {m}})...)
	if err != nil {
		t.Fatal(err)
	}
	s := Selection{Version: 1, Provider: "claude-code", Model: m, Auth: credential.Reference{Provider: "claude-code", Method: credential.OAuth, ID: "external-claude-code"}, MaxAttempts: 1, Source: "test"}
	c, _, err := r.Build(BuildConfig{Selection: s, ClaudeCode: claudecode.Config{Binary: f.Binary, Getenv: func(key string) string {
		if key == "HOME" {
			return f.Home
		}
		return ""
	}, Models: []modelcatalog.Model{{ID: m.ID, Efforts: []llm.ReasoningEffort{"medium"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := permission.WithPolicy(t.Context(), permission.Unrestricted())
	request := llm.Request{Model: llm.Model{ID: m.ID, ReasoningEffort: "medium"}, Input: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "wait"}}}}
	responded := make(chan error, 1)
	go func() {
		_, err := c.Respond(ctx, request, llm.RequestOptions{})
		responded <- err
	}()
	deadline := time.After(5 * time.Second)
	for len(f.Calls(t)) == 0 {
		select {
		case <-deadline:
			t.Fatal("Claude request did not start")
		case <-time.After(time.Millisecond):
		}
	}
	call := f.Calls(t)[0]
	closed := make(chan error, 1)
	go func() { closed <- c.Close() }()
	select {
	case err = <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-deadline:
		t.Fatal("adapter Close did not drain subprocess/pipe")
	}
	if err = <-responded; !errors.Is(err, context.Canceled) || ctx.Err() != nil {
		t.Fatal("Close did not cancel its own request independently", err)
	}
	for _, pid := range []int{call.PID, call.ChildPID} {
		if pid != 0 && !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			t.Fatal("adapter Close returned before process was reaped", pid)
		}
	}
	if _, err = c.Respond(ctx, request, llm.RequestOptions{}); !errors.Is(err, context.Canceled) || len(f.Calls(t)) != 1 {
		t.Fatal("closed adapter started another subprocess", err)
	}
}

//go:build linux || darwin

package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudecode"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

type claudeRequestRecorder struct {
	llm.Adapter
	requests chan<- llm.Request
}

func (r claudeRequestRecorder) Respond(ctx context.Context, req llm.Request, opt llm.RequestOptions) (llm.Response, error) {
	r.requests <- req
	return r.Adapter.Respond(ctx, req, opt)
}

func TestClaudeFailedToolRequestResumesCanonicalInput(t *testing.T) {
	t.Run("request schemas", func(t *testing.T) { testClaudeFailedInputResume(t, true) })
	t.Run("stream initialization", func(t *testing.T) { testClaudeFailedInputResume(t, false) })
}

func testClaudeFailedInputResume(t *testing.T, legacySchemas bool) {
	t.Helper()
	f := testclaude.New(t)
	f.Set(t, testclaude.Config{Subscription: "team"})
	initialTools, initialCalls := 1, 0
	diagnostic := "request included tool schemas"
	if !legacySchemas {
		// A rejected init creates the same typed failed-turn state as the old
		// catalog validator, without enabling or executing a Claude tool.
		f.Set(t, testclaude.Config{Subscription: "team", Stream: `{"type":"system","subtype":"init","model":"claude-configured-a","tools":["Bash"],"permissionMode":"default","mcp_servers":[]}` + "\n"})
		initialTools, initialCalls = 0, 1
		diagnostic = "Claude stream initialization violated isolation"
	}
	config := claudeServeConfiguration(t, f)
	config.Runtime.ClaudeCode.ManagedPolicyMode = claudecode.ManagedPolicyTrust
	config.Permissions.Tools = []string{}
	registry, err := runtimeProviders(config.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	store := t.TempDir()
	factory, identity, err := agentrunner.NewRuntimeFactory(agentrunner.RuntimeConfig{Identity: config.Runtime, Providers: registry, SessionDirectory: store})
	if err != nil {
		t.Fatal(err)
	}
	requests := make(chan llm.Request, 4)
	build := func(legacy bool) host.Factory {
		return func(ctx context.Context, id session.ID) (host.Runtime, error) {
			runtime, err := factory(ctx, id)
			if err == nil {
				if legacy {
					// Reproduce a previously failed request, not a new supported
					// capability. The replacement uses the unmodified factory.
					runtime.Builder.AddTool(llm.Tool{Type: llm.ToolFunction, Name: "Bash"})
				}
				runtime.LLM = claudeRequestRecorder{Adapter: runtime.LLM, requests: requests}
			}
			return runtime, err
		}
	}
	policy, err := permission.New(config.Permissions)
	if err != nil {
		t.Fatal(err)
	}
	defer policy.Close()
	owner, err := host.New(t.Context(), host.Config{Directory: store, Build: build(legacySchemas)})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	options := host.Options{ID: "failed-smoke", Lifecycle: "interactive", Workspace: config.Runtime.Workspace, Policy: policy, Configuration: identity}
	s, err := owner.Create(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	input := inbox.Input{ID: "original-smoke-input", Kind: inbox.InputExternal, Payload: []byte(`"Reply with exactly: offline smoke OK"`)}
	if _, err = s.Submit(t.Context(), s.Generation, input); err != nil {
		t.Fatal(err)
	}
	waitInteractive(t, func() bool {
		select {
		case <-s.Done():
			return true
		default:
			return false
		}
	})
	var typed *claudecode.Error
	if err = s.Wait(t.Context()); !errors.As(err, &typed) || typed.Code != "tools_unsupported" || !strings.Contains(err.Error(), diagnostic) {
		t.Fatal("legacy request did not reproduce the provider defense", err)
	}
	if !legacySchemas && !strings.HasPrefix(typed.Error(), "init_tools_nonempty(count=1): ") {
		t.Fatal("init failure lost its nonsecret branch diagnostic", err)
	}
	if len(requests) != 1 || len((<-requests).Tools) != initialTools || len(f.Calls(t)) != initialCalls {
		t.Fatal("rejection crossed the wrong boundary or lost the original request")
	}
	failed, err := s.Inspect(0, 256)
	if err != nil || len(failed.Operations) != 0 || responseCount(failed) != 0 {
		t.Fatal("failed schema request created model output or an Operation", err)
	}
	if !legacySchemas && !strings.Contains(failed.Failure, "init_tools_nonempty(count=1)") {
		t.Fatal("canonical failure projection lost its branch diagnostic")
	}
	if err = owner.Close(); err != nil {
		t.Fatal(err)
	}
	// The normal 2.1.285 fixture includes default permission mode, empty
	// tools/MCP, agent/skill catalogs, status and allowed rate-limit metadata.
	f.Set(t, testclaude.Config{Subscription: "team"})
	replacement, err := host.New(t.Context(), host.Config{Directory: store, Build: build(false)})
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	s, err = replacement.Resume(t.Context(), options)
	if err != nil {
		t.Fatal("failed smoke requires no new Session or configuration rewrite", err)
	}
	var resumed host.View
	waitInteractive(t, func() bool {
		resumed, err = s.Inspect(0, 256)
		if err != nil {
			t.Fatal(err)
		}
		if !resumed.Running {
			t.Fatal("text-only recovery did not reach its fake response", resumed.Failure)
		}
		return responseCount(resumed) == 1
	})
	if len(requests) != 1 {
		t.Fatal("resume did not perform exactly one model request")
	}
	req := <-requests
	if len(req.Tools) != 0 {
		t.Fatal("Host -> Coordinator -> runtime -> provider included tool schemas")
	}
	if len(resumed.Operations) != 0 || len(f.Calls(t)) != initialCalls+1 {
		t.Fatal("text-only resume executed a tool or duplicate model request")
	}
	if len(resumed.History.Items) < len(failed.History.Items) || !reflect.DeepEqual(resumed.History.Items[:len(failed.History.Items)], failed.History.Items) {
		t.Fatal("resume rewrote failed canonical history")
	}
	var turns []session.Turn
	inputs := 0
	for _, item := range resumed.History.Items {
		switch record := item.Data.(type) {
		case inbox.Input:
			if record.Kind == inbox.InputExternal {
				inputs++
				if record.ID != input.ID {
					t.Fatal("resume replaced the original input ID")
				}
			}
		case session.Turn:
			turns = append(turns, record)
		case sessionstore.ToolCallStatus:
			t.Fatal("text-only turn reached a Translator or canonical tool Operation")
		}
	}
	if inputs != 1 || len(turns) != 2 || turns[1].PreviousTurnID != turns[0].ID || turns[0].RuntimeRevision != turns[1].RuntimeRevision {
		t.Fatal("resume did not retain the failed turn and its model revision")
	}
	call := f.Calls(t)[initialCalls]
	cliArgument(t, call, "--tools", "")
	cliArgument(t, call, "--disallowedTools", "*")
	cliArgument(t, call, "--mcp-config", `{"mcpServers":{}}`)
	for _, unavailable := range []string{"prefer to go wider with tool calls", "issue them as separate tool calls", "Tool calls are asynchronous"} {
		if strings.Contains(call.System, unavailable) {
			t.Fatal("Claude received instructions for unavailable tools")
		}
	}
	if !strings.Contains(call.System, "The host keeps the session alive until an explicit stop.") {
		t.Fatal("text-only runtime changed interactive lifecycle guidance")
	}
	if _, err = s.Stop(t.Context(), s.Generation, inbox.StopWhenIdle, "offline test complete"); err != nil {
		t.Fatal(err)
	}
}

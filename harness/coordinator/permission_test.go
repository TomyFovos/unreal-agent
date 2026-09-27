package coordinator

import (
	"context"
	"encoding/json/jsontext"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"github.com/unreallabsai/unreal-agent/harness/tool/bash"
)

func TestDeniedToolDoesNotTranslateOrDispatch(t *testing.T) {
	spec, err := operation.NewValueSpec(jsontext.Value("42"))
	if err != nil {
		t.Fatal(err)
	}
	translator := &submittingTranslator{specs: []operation.Spec{spec}}
	store := emptyFakeStore()
	current := newTestCoordinator(store, newTestInbox(t), newFakeOperationManager(), contextbuilder.NewBuilder(), tool.NewRegistry(tool.StaticTranslators{Bash: translator}, tool.BashName))
	statuses, err := current.handleModelResponse(permission.WithPolicy(t.Context(), permission.DenyAll()), sessionstore.ModelResponse{
		TurnID: "turn", Response: llm.Response{Output: []llm.Item{{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "call", Name: tool.BashName}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || statuses[0].Status.Denial == nil || len(translator.calls) != 0 || len(statuses[0].Operations) != 0 {
		t.Fatalf("denied call=%#v", statuses)
	}
}

func TestHiddenToolHistoryStillRestores(t *testing.T) {
	store := emptyFakeStore()
	store.items = []sessionstore.Item{
		storedItem(1, sessionstore.ItemTurn, session.Turn{ID: "turn", Type: session.TurnRegular}),
		storedItem(2, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "turn", Response: llm.Response{Output: []llm.Item{{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "call", Name: tool.ViewImageName}}}}}),
		storedItem(3, sessionstore.ItemToolCallStatus, sessionstore.ToolCallStatus{TurnID: "turn", CallID: "call", Status: tool.CallStatus{}}),
	}
	registry := tool.NewRegistry(tool.StaticTranslators{ViewImage: testTranslator{}})
	if _, ok := registry.Resolve(tool.ViewImageName); ok {
		t.Fatal("hidden tool is executable")
	}
	current := newTestCoordinator(store, newTestInbox(t), newFakeOperationManager(), contextbuilder.NewBuilder(), registry)
	if err := current.restore(permission.WithPolicy(t.Context(), permission.DenyAll())); err != nil {
		t.Fatal(err)
	}
	if len(current.state.toolCalls) != 0 {
		t.Fatal("historical result was not formatted")
	}
}

func TestCoordinatorContinuesAfterExecutorPermissionFailure(t *testing.T) {
	policy, err := permission.New(permission.Config{Tools: []string{"Bash"}})
	if err != nil {
		t.Fatal(err)
	}
	defer policy.Close()
	base := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 0)
		ctx, cancel := context.WithCancel(permission.WithPolicy(t.Context(), policy))
		defer cancel()
		manager := operation.NewLocalOperationManagerWithPolicy(ctx, policy)
		run.current.dependencies.Operations = manager
		run.current.dependencies.Tools = tool.NewRegistry(tool.StaticTranslators{Bash: bash.New(bash.Config{Shell: "/bin/sh", BaseDirectory: base})}, tool.BashName)
		go func() { run.done <- run.current.Run(ctx) }()
		synctest.Wait()
		run.input(t, externalEvent(t, 0, "first", "try command"))
		run.respond(t, 0, llm.Response{Output: []llm.Item{{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "denied", Name: tool.BashName, Arguments: `{"command":"touch should-not-exist"}`}}}})
		if len(run.calls) != 2 {
			t.Fatalf("corrective requests=%d", len(run.calls))
		}
		found := false
		for _, item := range run.calls[1].request.Input {
			if result, ok := item.Data.(llm.ToolResult); ok && result.CallID == "denied" {
				for _, output := range result.Output {
					if strings.Contains(output.Value, "permission denied") {
						found = true
					}
				}
			}
		}
		if !found {
			t.Fatal("model did not receive permission result")
		}
		run.respond(t, 1, textResponse("Permission denied."))
		run.assertRunning(t)
		run.input(t, externalEvent(t, 1, "second", "continue"))
		run.respond(t, 2, textResponse("Still available."))
		run.input(t, stopInput(t, "stop", inbox.StopWhenIdle))
		run.assertStopped(t)
	})
}

package coordinator

import (
	"encoding/json/jsontext"
	"testing"
	"testing/synctest"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

type messagesBatchAdapter struct{ llm.Adapter }

func (messagesBatchAdapter) RequiresFullToolBatch() bool { return true }

func messagesRequireBatch(run *stopTestRun) {
	run.current.dependencies.LLM = messagesBatchAdapter{run.current.dependencies.LLM}
}

// The legacy async fixture uses shell-shaped placeholders without a typed shell
// receipt. Use actual inert Value plans here, rather than pretending those
// placeholders prove a successful process execution.
func messagesUseValueOperations(t *testing.T, run *stopTestRun) {
	t.Helper()
	spec, err := operation.NewValueSpec(jsontext.Value(`42`))
	if err != nil {
		t.Fatal(err)
	}
	convert := func(op operation.Operation) operation.Operation {
		op.Type, op.Version, op.State = spec.Type, spec.Version, spec.State
		return op
	}
	for i, op := range run.store.resume.Operations {
		run.store.resume.Operations[i] = convert(op)
	}
	for i, item := range run.store.items {
		if status, ok := item.Data.(sessionstore.ToolCallStatus); ok {
			for j, op := range status.Operations {
				status.Operations[j] = convert(op)
			}
			item.Data = status
			run.store.items[i] = item
		}
	}
	run.current.dependencies.Restored = run.store.resume
}

func messagesResult(t *testing.T, request llm.Request, id string) llm.ToolResult {
	t.Helper()
	var found *llm.ToolResult
	for _, item := range request.Input {
		if result, ok := item.Data.(llm.ToolResult); ok && result.CallID == id {
			if found != nil {
				t.Fatal("duplicate result remained in request projection")
			}
			copy := result
			found = &copy
		}
	}
	if found == nil {
		t.Fatal("canonical tool result absent from request projection")
	}
	return *found
}

func TestMessagesFullBatchWaitsForEveryTerminalResult(t *testing.T) {
	for _, terminal := range []operation.Status{operation.StatusCompleted, operation.StatusFailed, operation.StatusCanceled} {
		t.Run(string(terminal), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				run := newStopTestRun(t, 2)
				messagesUseValueOperations(t, run)
				messagesRequireBatch(run)
				run.start(t)
				initialDispatches := len(run.operations.adds)
				if len(run.calls) != 0 || initialDispatches != 2 {
					t.Fatal("restore did not recover the pending Operations or began early inference")
				}
				// Complete the second call first, then supply unrelated user/control
				// inputs and expire the legacy grace timer while the first is pending.
				run.update(t, 1, terminal)
				run.input(t, externalEvent(t, 0, "queued-input", "synthetic queued user input"))
				run.input(t, heartbeatInput(t, "queued-heartbeat"))
				synctest.Sleep(2 * time.Second)
				synctest.Wait()
				if len(run.calls) != 0 {
					t.Fatal("partial results, heartbeat, user input, or grace timeout broke full batch barrier")
				}
				run.update(t, 0, operation.StatusCompleted)
				if len(run.calls) != 1 {
					t.Fatal("all terminal results did not release exactly one generation")
				}
				first, second := messagesResult(t, run.calls[0].request, "call-0"), messagesResult(t, run.calls[0].request, "call-1")
				wantSecond := llm.ToolResultSucceeded
				if terminal != operation.StatusCompleted {
					wantSecond = llm.ToolResultFailed
				}
				if first.State != llm.ToolResultSucceeded || second.State != wantSecond || first.Output[0].Value != string(operation.StatusCompleted) || second.Output[0].Value != string(terminal) {
					t.Fatal("reverse completion order lost typed state or result identity")
				}
				if len(run.operations.adds) != initialDispatches || len(run.store.appendedStatuses) != 2 {
					t.Fatalf("new dispatch or duplicated terminal receipt: initial=%d final=%d receipts=%d", initialDispatches, len(run.operations.adds), len(run.store.appendedStatuses))
				}
				run.input(t, stopInput(t, "stop", inbox.StopWhenIdle))
				run.respond(t, 0, textResponse("synthetic final"))
				run.assertStopped(t)
			})
		})
	}
}

func TestMessagesFullBatchBarrierIncludesRestoredPartialBatch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 2)
		messagesUseValueOperations(t, run)
		// Persisted canonical receipt for the second call already exists while
		// the first Operation remains pending at Host restart.
		completed := run.store.resume.Operations[1]
		completed.Status = operation.StatusCompleted
		run.store.resume.Operations[1] = completed
		run.store.items = append(run.store.items, storedItem(5, sessionstore.ItemToolCallStatus, sessionstore.ToolCallStatus{
			TurnID: "turn-1", CallID: "call-1", Status: tool.CallStatus{WaitingFor: []operation.ID{completed.ID}}, Operations: []operation.Operation{completed},
		}))
		run.current.dependencies.Restored = run.store.resume
		messagesRequireBatch(run)
		run.start(t)
		if len(run.calls) != 0 {
			t.Fatal("startup generation bypassed restored partial batch barrier")
		}
		run.update(t, 0, operation.StatusCompleted)
		if len(run.calls) != 1 || messagesResult(t, run.calls[0].request, "call-0").State != llm.ToolResultSucceeded || messagesResult(t, run.calls[0].request, "call-1").State != llm.ToolResultSucceeded {
			t.Fatal("resumed batch did not release complete typed results")
		}
		run.input(t, stopInput(t, "stop", inbox.StopWhenIdle))
		run.respond(t, 0, textResponse("synthetic final"))
		run.assertStopped(t)
	})
}

func TestMessagesBarrierDoesNotChangeLegacyAsyncContinuation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 2)
		run.start(t)
		run.update(t, 1, operation.StatusCompleted)
		if len(run.calls) != 1 {
			t.Fatal("legacy adapter no longer receives partial async continuation")
		}
		if messagesResult(t, run.calls[0].request, "call-0").State != llm.ToolResultUnknown || messagesResult(t, run.calls[0].request, "call-1").State != llm.ToolResultUnknown {
			t.Fatal("native-only typed projection altered legacy request contract")
		}
		run.update(t, 0, operation.StatusCompleted)
		run.input(t, stopInput(t, "stop", inbox.StopWhenIdle))
		run.respond(t, 0, textResponse("synthetic intermediate"))
		if len(run.calls) != 2 {
			t.Fatal("legacy late result was not delivered")
		}
		run.respond(t, 1, textResponse("synthetic final"))
		run.assertStopped(t)
	})
}

func TestMessagesTypedPermissionDenialCreatesNoOperation(t *testing.T) {
	spec, err := operation.NewValueSpec(jsontext.Value(`42`))
	if err != nil {
		t.Fatal(err)
	}
	translator := &submittingTranslator{specs: []operation.Spec{spec}}
	store := emptyFakeStore()
	manager := newFakeOperationManager()
	current := newTestCoordinator(store, newTestInbox(t), manager, contextbuilder.NewBuilder(), tool.NewRegistry(tool.StaticTranslators{Bash: translator}, tool.BashName))
	current.dependencies.LLM = messagesBatchAdapter{current.dependencies.LLM}
	statuses, err := current.handleModelResponse(permission.WithPolicy(t.Context(), permission.DenyAll()), sessionstore.ModelResponse{
		TurnID: "synthetic-turn", Response: llm.Response{Output: []llm.Item{{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "denied", Name: tool.BashName, Arguments: `{}`}}}},
	})
	if err != nil || len(statuses) != 1 || len(store.appendedStatuses) != 1 || store.appendedStatuses[0].Status.Denial == nil || len(translator.calls) != 0 || len(manager.adds) != 0 || len(statuses[0].Operations) != 0 {
		t.Fatal("denied tool bypassed typed Permission or reached execution")
	}
	request, err := current.dependencies.ContextBuilder.Build()
	if err != nil || messagesResult(t, request.Request, "denied").State != llm.ToolResultFailed {
		t.Fatal("denial was not preserved as Host-derived failed ToolResult")
	}
}

func TestMessagesTypedStateReconstructedFromCanonicalOnResume(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 1)
		messagesUseValueOperations(t, run)
		store := persistTestRun(t, run)
		restoreTestRun(t, run, store)
		messagesRequireBatch(run)
		run.start(t)
		run.update(t, 0, operation.StatusCompleted)
		if len(run.calls) != 1 || messagesResult(t, run.calls[0].request, "call-0").State != llm.ToolResultSucceeded {
			t.Fatal("live canonical terminal result not classified")
		}
		run.input(t, stopInput(t, "stop", inbox.StopWhenIdle))
		run.respond(t, 0, textResponse("synthetic final"))
		run.assertStopped(t)
		resumed := newStopTestRun(t, 0)
		restoreTestRun(t, resumed, store)
		messagesRequireBatch(resumed)
		if err := resumed.current.restore(t.Context()); err != nil {
			t.Fatal("cannot reconstruct synthetic canonical history")
		}
		request, err := resumed.current.dependencies.ContextBuilder.Build()
		if err != nil || messagesResult(t, request.Request, "call-0").State != llm.ToolResultSucceeded || len(resumed.operations.adds) != 0 {
			t.Fatal("resume lost typed state or reexecuted terminal Operation")
		}
		page, err := store.Items(t.Context(), "session-1", sessionstore.BeforeFirst, 100)
		if err != nil {
			t.Fatal("cannot read synthetic canonical records")
		}
		terminalReceipts := 0
		for _, item := range page.Items {
			if status, ok := item.Data.(sessionstore.ToolCallStatus); ok && status.CallID == "call-0" && len(status.Operations) == 1 && status.Operations[0].Status == operation.StatusCompleted {
				terminalReceipts++
			}
		}
		if terminalReceipts != 1 {
			t.Fatal("canonical terminal receipt missing or duplicated across resume")
		}
	})
}

func TestMessagesTerminalResultStateDoesNotGuessUnknownSuccess(t *testing.T) {
	if got := terminalResultState(sessionstore.ToolCallStatus{}, nil, false); got != llm.ToolResultUnknown {
		t.Fatal("unclassified zero-value result became success")
	}
	if got := terminalResultState(sessionstore.ToolCallStatus{}, nil, true); got != llm.ToolResultPending {
		t.Fatal("running result did not remain pending")
	}
	if got := terminalResultState(sessionstore.ToolCallStatus{Operations: []operation.Operation{{Status: operation.StatusAwaiting}}}, operationStatusTranslator{}, false); got != llm.ToolResultPending {
		t.Fatal("nonterminal canonical Operation became successful result")
	}
	if got := terminalResultState(sessionstore.ToolCallStatus{Operations: []operation.Operation{{Type: operation.TypeShell, Status: operation.StatusCompleted}}}, operationStatusTranslator{}, false); got != llm.ToolResultUnknown {
		t.Fatal("missing typed shell result became successful result")
	}
}

var _ llm.Adapter = messagesBatchAdapter{}

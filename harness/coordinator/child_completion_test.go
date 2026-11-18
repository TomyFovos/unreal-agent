package coordinator

import (
	"encoding/json/v2"
	"errors"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
)

func TestStandaloneChildCompletionWakesBeforeWhenIdleStop(t *testing.T) {
	for _, provider := range []string{"responses", "claude-without-hook", "claude-at-hook"} {
		for _, status := range []operation.Status{operation.StatusCompleted, operation.StatusFailed, operation.StatusCanceled} {
			t.Run(string(status)+"/"+provider, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					run := newStopTestRun(t, 0)
					projected := 0
					run.current.dependencies.CompletedInput = func(op operation.Operation) (*inbox.Input, error) {
						projected++
						if op.Status != status {
							t.Fatal("projected nonterminal child", op.Status)
						}
						input := progressPeerInput(t, inbox.ID("ended:"+string(op.ID)), "ended", string(op.ID)+" ended: "+string(op.Status)+" stop=max_output_tokens")
						return &input, nil
					}
					run.start(t)
					run.input(t, externalEvent(t, 0, "task", "delegate"))
					opt := run.current.dependencies.LLM.(*fakeAdapter).requestOptionsSnapshot()[0]
					if provider != "responses" {
						if err := opt.BeginTools(run.calls[0].ctx, true); err != nil {
							t.Fatal(err)
						}
					}
					spec, err := operation.NewRemoteJobSpec(operation.RemoteJobPlan{Type: "subagent", Version: 1, Data: []byte(`{"Action":"start","Notify":true}`)})
					if err != nil {
						t.Fatal(err)
					}
					raw, _ := json.Marshal(spec)
					for _, id := range []string{"child-one", "child-two"} {
						payload, _ := json.Marshal(inbox.ControlMessage{Mode: inbox.DispatchOperation, Parameters: inbox.OperationIntent{OperationID: id, ToolName: "SubagentStart", Spec: raw}})
						run.input(t, inbox.Input{ID: inbox.ID("start:" + id), Kind: inbox.InputControl, Payload: payload})
					}
					run.input(t, stopInput(t, "stop", inbox.StopWhenIdle))
					var updates []operation.Operation
					for _, id := range []operation.ID{"child-one", "child-two"} {
						op := run.current.state.operations[id]
						op.Status = status
						updates = append(updates, op)
					}
					for _, op := range updates {
						run.operations.updates <- op
					}
					synctest.Wait()
					synctest.Sleep(2 * slurpIdleTimeout)
					synctest.Wait()
					if projected != 0 {
						t.Fatal("projected before active model could read the result")
					}
					if provider == "claude-at-hook" {
						inputs, err := opt.Steering(run.calls[0].ctx)
						if err != nil || len(inputs) != 2 || projected != 2 || !inputs[0].Peer || !inputs[1].Peer {
							t.Fatalf("terminal child notices absent at the boundary: %+v, %v", inputs, err)
						}
						if text := inputs[0].Text + "\n" + inputs[1].Text; strings.Count(text, "child-one ended:") != 1 || strings.Count(text, "child-two ended:") != 1 {
							t.Fatalf("batch lost or repeated child notices: %s", text)
						}
						synctest.Sleep(2 * slurpIdleTimeout) // finish the hook's event phase
						synctest.Wait()
						run.respond(t, 0, llm.Response{})
						run.assertStopped(t)
						if run.modelCallCount() != 1 || len(run.store.appendedInputs) != 6 {
							t.Fatal("delivered child notices were lost or woke an additional turn")
						}
						return
					}
					run.respond(t, 0, textResponse("delegated"))
					run.assertRunning(t)
					if projected != 2 || len(run.store.appendedInputs) != 6 {
						t.Fatal("lost durable completion notice", projected, len(run.store.appendedInputs))
					}
					if run.modelCallCount() != 2 || !strings.Contains(requestDump(t, run.calls[1].request), "stop=max_output_tokens") {
						t.Fatalf("parent did not wake immediately with reason: calls=%d wake=%d pending=%d", run.modelCallCount(), run.current.wakingInputs(), run.current.pendingInputs())
					}
					for _, id := range []string{"child-one", "child-two"} {
						if strings.Count(requestDump(t, run.calls[1].request), id+" ended:") != 1 {
							t.Fatal("turn lost or repeated a child report", id)
						}
					}
					run.respond(t, 1, textResponse("final review"))
					run.assertStopped(t)
				})
			})
		}
	}
}

func TestStandaloneCompletionSurvivesCrashBeforeNoticeAndIsNotRepeated(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 0)
		store := persistTestRun(t, run)
		spec, err := operation.NewRemoteJobSpec(operation.RemoteJobPlan{Type: "subagent", Version: 1, Data: []byte(`{"Action":"start","Notify":true}`)})
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(spec)
		payload, _ := json.Marshal(inbox.ControlMessage{Mode: inbox.DispatchOperation, Parameters: inbox.OperationIntent{OperationID: "child", ToolName: "SubagentStart", Spec: raw}})
		if err = store.AppendInput(t.Context(), "session-1", inbox.Input{ID: "start", Kind: inbox.InputControl, Payload: payload}); err != nil {
			t.Fatal(err)
		}
		op := operation.Operation{ID: "child", Type: spec.Type, Version: spec.Version, State: spec.State, MaxOutputLength: spec.MaxOutputLength, Status: operation.StatusReady}
		state, _ := operation.DecodeRemoteJobState(op)
		state.TerminalError = "stop=max_output_tokens"
		step, err := operation.UpdateRemoteJob(op, state, operation.StatusFailed)
		if err != nil {
			t.Fatal(err)
		}
		if err = store.SaveOperation(t.Context(), "session-1", *step.Operation); err != nil {
			t.Fatal(err)
		}
		restoreTestRun(t, run, store)
		crash := errors.New("crash before completion notice")
		run.current.dependencies.CompletedInput = func(operation.Operation) (*inbox.Input, error) { return nil, crash }
		run.start(t)
		if err = <-run.done; !errors.Is(err, crash) {
			t.Fatal(err)
		}
		project := func(op operation.Operation) (*inbox.Input, error) {
			if op.Status != operation.StatusFailed {
				t.Fatal("restoration changed terminal result", op.Status)
			}
			input := progressPeerInput(t, "ended:child", "ended", "stop=max_output_tokens")
			return &input, nil
		}
		resumed := newStopTestRun(t, 0)
		restoreTestRun(t, resumed, store)
		resumed.current.dependencies.CompletedInput = project
		resumed.current.dependencies.RestoredStop = &inbox.ControlMessage{Mode: inbox.StopWhenIdle}
		resumed.start(t)
		resumed.assertRunning(t)
		if resumed.modelCallCount() != 1 || !strings.Contains(requestDump(t, resumed.calls[0].request), "stop=max_output_tokens") {
			t.Fatal("restored notice did not wake parent")
		}
		resumed.respond(t, 0, textResponse("reviewed"))
		resumed.assertStopped(t)
		again := newStopTestRun(t, 0)
		restoreTestRun(t, again, store)
		again.current.dependencies.CompletedInput = project
		again.current.dependencies.RestoredStop = &inbox.ControlMessage{Mode: inbox.StopWhenIdle}
		again.start(t)
		again.assertStopped(t)
		if again.modelCallCount() != 0 {
			t.Fatal("already delivered completion caused another turn")
		}
		page, err := store.Items(t.Context(), "session-1", 0, 128)
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, item := range page.Items {
			if input, ok := item.Data.(inbox.Input); ok && input.ID == "ended:child" {
				count++
			}
		}
		if count != 1 {
			t.Fatal("completion duplicated across restart", count)
		}
	})
}

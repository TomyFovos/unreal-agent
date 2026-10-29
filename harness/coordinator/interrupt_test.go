package coordinator

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"slices"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

func TestRestoredEndedNoticeStartsBackgroundTurnAndYieldsToUser(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 0)
		peer, _ := json.Marshal(inbox.PeerMessage{Version: 1, Sender: "child", Target: "session-1", Handle: "h", Kind: "ended", Text: "Child h stopped because Unreal Agent restarted."})
		run.store.items = []sessionstore.Item{
			storedItem(1, sessionstore.ItemInput, externalEvent(t, 0, "task", "delegate")),
			storedItem(2, sessionstore.ItemTurn, session.Turn{ID: "original", Type: session.TurnRegular}),
			storedItem(3, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "original", Response: textResponse("delegated")}),
			storedItem(4, sessionstore.ItemInput, inbox.Input{ID: "restarted:h", Kind: inbox.InputPeer, Payload: peer}),
		}
		run.start(t)
		if len(run.calls) != 1 || !run.current.backgroundTurn {
			t.Fatal("restored ended message did not wake a background turn")
		}
		options := run.current.dependencies.LLM.(*fakeAdapter).requestOptionsSnapshot()[0]
		if err := options.BeginTools(run.calls[0].ctx, false); err != nil {
			t.Fatal(err)
		}
		run.input(t, externalEvent(t, 1, "user", "answer me first"))
		if len(run.calls) != 2 || run.calls[0].ctx.Err() == nil || run.current.backgroundTurn {
			t.Fatal("user input did not replace the restored background turn")
		}
		run.respond(t, 1, textResponse("answered"))
	})
}

// /stop sub cancels only child agents and /stop main only the agent's own
// operations.
func TestStopMainAndSubCancelSeparateOperations(t *testing.T) {
	store := emptyFakeStore()
	manager := newFakeOperationManager()
	c := newTestCoordinator(store, newTestInbox(t), manager, contextbuilder.NewBuilder(), tool.NewRegistry(tool.StaticTranslators{}))
	for id, plan := range map[operation.ID]string{"child": "subagent", "own": "native_file"} {
		spec, err := operation.NewRemoteJobSpec(operation.RemoteJobPlan{Type: operation.RemoteJobPlanType(plan), Version: 1, Data: jsontext.Value("{}")})
		if err != nil {
			t.Fatal(err)
		}
		c.state.operations[id] = operation.Operation{ID: id, Type: spec.Type, Version: spec.Version, State: spec.State, MaxOutputLength: spec.MaxOutputLength, Status: operation.StatusAwaiting}
	}
	control := func(mode inbox.ControlMode) {
		data, _ := json.Marshal(inbox.ControlMessage{Mode: mode, Reason: "terminal user requested stop"})
		if err := c.handleInboxInput(t.Context(), inbox.Input{ID: inbox.ID(mode), Kind: inbox.InputControl, Payload: data}); err != nil {
			t.Fatal(err)
		}
	}
	control(inbox.CancelChildren)
	if !slices.Equal(manager.cancels, []operation.ID{"child"}) {
		t.Fatalf("/stop sub canceled %v, want the child only", manager.cancels)
	}
	control(inbox.InterruptAgent)
	if !slices.Equal(manager.cancels, []operation.ID{"child", "own"}) {
		t.Fatalf("/stop main canceled %v, want the agent's own operation", manager.cancels)
	}
	if !c.held() {
		t.Fatal("/stop main did not hold the model for the user")
	}
}

func TestForegroundChildWaitYieldsToUserInput(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 0)
		run.start(t)
		run.input(t, externalEvent(t, 0, "delegate", "delegate the work"))
		if run.current.backgroundTurn {
			t.Fatal("the test must start a foreground turn")
		}
		options := run.current.dependencies.LLM.(*fakeAdapter).requestOptionsSnapshot()[0]
		if err := options.BeginTools(run.calls[0].ctx, true); err != nil {
			t.Fatal(err)
		}
		run.input(t, externalEvent(t, 1, "steering", "answer this first"))
		if run.calls[0].ctx.Err() == nil || len(run.calls) != 2 {
			t.Fatal("child wait was not interrupted and replaced by a user turn")
		}
		if !strings.Contains(requestDump(t, run.calls[1].request), "answer this first") || run.current.backgroundTurn || run.current.waitingForChildren {
			t.Fatal("replacement turn did not carry the user's input or retained wait state")
		}
		run.respond(t, 1, textResponse("Answered."))
		run.input(t, stopInput(t, "stop", inbox.StopWhenIdle))
		run.assertStopped(t)
	})
}

func TestChildWaitImmediatelyYieldsToAlreadyQueuedUserInput(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 0)
		run.start(t)
		run.input(t, externalEvent(t, 0, "task", "delegate"))
		opt := run.current.dependencies.LLM.(*fakeAdapter).requestOptionsSnapshot()[0]
		if err := opt.BeginTools(run.calls[0].ctx, false); err != nil {
			t.Fatal(err)
		}
		run.input(t, externalEvent(t, 1, "queued", "answer first"))
		if len(run.calls) != 1 {
			t.Fatal("foreground turn was interrupted before waiting")
		}
		if err := opt.BeginTools(run.calls[0].ctx, true); err == nil {
			t.Fatal("wait accepted despite pending user input")
		}
		synctest.Wait()
		synctest.Sleep(2 * slurpIdleTimeout)
		synctest.Wait()
		if run.calls[0].ctx.Err() == nil || len(run.calls) != 2 || !strings.Contains(requestDump(t, run.calls[1].request), "answer first") {
			t.Fatal("queued input did not immediately interrupt the wait")
		}
		run.respond(t, 1, textResponse("answered"))
	})
}

func TestSteeredInputDoesNotInterruptALaterChildWaitOrStartAnotherTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 0)
		run.start(t)
		run.input(t, externalEvent(t, 0, "task", "work"))
		opt := run.current.dependencies.LLM.(*fakeAdapter).requestOptionsSnapshot()[0]
		if err := opt.BeginTools(run.calls[0].ctx, false); err != nil {
			t.Fatal(err)
		}
		run.input(t, externalEvent(t, 1, "queued", "answer now"))
		inputs, err := opt.Steering(run.calls[0].ctx)
		if err != nil || !slices.Equal(inputs, []string{"answer now"}) {
			t.Fatal("steering", inputs, err)
		}
		if err := opt.BeginTools(run.calls[0].ctx, true); err != nil || run.calls[0].ctx.Err() != nil {
			t.Fatal("already delivered input interrupted a later wait", err)
		}
		run.respond(t, 0, textResponse("answered"))
		if len(run.calls) != 1 || run.current.pendingInputs() != 0 {
			t.Fatal("steered input was resent")
		}
	})
}

func TestCompletedChildWaitKeepsForegroundWorkRunning(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 0)
		run.start(t)
		run.input(t, externalEvent(t, 0, "task", "work"))
		options := run.current.dependencies.LLM.(*fakeAdapter).requestOptionsSnapshot()[0]
		for _, waiting := range []bool{true, false} {
			if err := options.BeginTools(run.calls[0].ctx, waiting); err != nil {
				t.Fatal(err)
			}
		}
		run.input(t, externalEvent(t, 1, "queued", "follow-up"))
		if run.calls[0].ctx.Err() != nil || len(run.calls) != 1 {
			t.Fatal("a completed wait made later foreground work interruptible")
		}
		run.respond(t, 0, llm.Response{})
		run.respond(t, 1, textResponse("Done."))
	})
}

// /stop main ends the model call and the agent's tools, and the model waits
// for the user's next input, which carries what happened meanwhile.
func TestStopMainWaitsForTheUser(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 1)
		run.start(t)
		run.input(t, externalEvent(t, 1, "work", "check progress"))
		run.input(t, stopInput(t, "interrupt", inbox.InterruptAgent))
		if run.calls[0].ctx.Err() == nil {
			t.Fatal("/stop main did not interrupt the model call")
		}
		if len(run.operations.cancels) != 1 {
			t.Fatalf("cancellations = %v, want the running tool", run.operations.cancels)
		}
		run.update(t, 0, operation.StatusCanceled)
		run.assertRunning(t)
		if len(run.calls) != 1 {
			t.Fatal("the canceled tool called the model before the user spoke")
		}
		run.input(t, externalEvent(t, 2, "next", "do something else"))
		if len(run.calls) != 2 {
			t.Fatalf("requests = %d, want the user's next input to call the model", len(run.calls))
		}
		assertStopResult(t, run.calls[1].request, "call-0", string(operation.StatusCanceled))
		if !strings.Contains(requestDump(t, run.calls[1].request), "do something else") {
			t.Fatal("the next request lacks the user's input")
		}
		run.respond(t, 1, textResponse("Done."))
		run.assertRunning(t)
	})
}

func requestDump(t *testing.T, request any) string {
	t.Helper()
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

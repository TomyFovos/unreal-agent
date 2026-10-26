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
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

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

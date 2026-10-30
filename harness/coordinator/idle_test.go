package coordinator

import (
	"context"
	"encoding/json/v2"
	"testing"
	"testing/synctest"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
)

type idleTestAdapter struct {
	llm.Adapter
	idle     []context.Context
	complete chan struct{}
}

func (a *idleTestAdapter) Idle(ctx context.Context, _ llm.Request, _ llm.RequestOptions) {
	a.idle = append(a.idle, ctx)
	select {
	case <-ctx.Done():
	case <-a.complete:
	}
}

func TestIdleMaintenanceYieldsOnlyToUserTurns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 0)
		adapter := &idleTestAdapter{Adapter: run.current.dependencies.LLM, complete: make(chan struct{})}
		run.current.dependencies.LLM = adapter
		run.start(t)
		if len(adapter.idle) != 0 {
			t.Fatal("maintenance ran before a response")
		}
		run.input(t, externalEvent(t, 0, "first", "first task"))
		if len(adapter.idle) != 0 {
			t.Fatal("maintenance ran during Respond")
		}
		run.respond(t, 0, textResponse("first done"))
		if len(adapter.idle) != 1 || adapter.idle[0].Err() != nil {
			t.Fatal("no idle maintenance after Respond returned")
		}
		run.input(t, settingsInput(t, "effort", inbox.Settings{ReasoningEffort: llm.ReasoningEffortHigh}))
		if len(adapter.idle) != 1 || adapter.idle[0].Err() != nil || len(run.calls) != 1 {
			t.Fatal("settings interrupted maintenance or started a model turn")
		}
		run.input(t, progressPeerInput(t, "progress", "progress", "tests still running"))
		if len(adapter.idle) != 1 || adapter.idle[0].Err() != nil || len(run.calls) != 1 {
			t.Fatal("child progress interrupted maintenance or started a model turn")
		}
		run.input(t, externalEvent(t, 1, "second", "second task"))
		if len(run.calls) != 2 || adapter.idle[0].Err() == nil {
			t.Fatal("new user turn did not cancel and join maintenance")
		}
		run.respond(t, 1, textResponse("second done"))
		if len(adapter.idle) != 2 {
			t.Fatal("maintenance did not restart at the next idle")
		}
		payload, err := json.Marshal(inbox.PeerMessage{Version: 1, Sender: "child", Target: "session-1", Handle: "h", Kind: "ended", Text: "Child h ended; review the result."})
		if err != nil {
			t.Fatal(err)
		}
		run.input(t, inbox.Input{ID: "child-ended", Kind: inbox.InputPeer, Payload: payload})
		if len(run.calls) != 2 || adapter.idle[1].Err() != nil {
			t.Fatal("child notification interrupted maintenance")
		}
		run.input(t, progressPeerInput(t, "message", "message", "child result"))
		if len(run.calls) != 2 || adapter.idle[1].Err() != nil {
			t.Fatal("child message interrupted maintenance")
		}
		adapter.complete <- struct{}{}
		synctest.Wait()
		synctest.Sleep(2 * slurpIdleTimeout)
		synctest.Wait()
		if len(run.calls) != 3 || !run.current.backgroundTurn {
			t.Fatal("queued child inputs did not start a turn after maintenance")
		}
		if run.current.state.currentTurnInputs != run.current.state.availableInputs {
			t.Fatal("queued child inputs were not included in the turn")
		}
		run.respond(t, 2, textResponse("reviewed"))
		if len(adapter.idle) != 3 {
			t.Fatal("maintenance did not restart after the child turn")
		}
		run.input(t, stopInput(t, "stop", inbox.StopWhenIdle))
		run.assertStopped(t)
		if adapter.idle[2].Err() == nil {
			t.Fatal("Session stop left maintenance running")
		}
	})
}

func TestIdleMaintenanceWaitsForTools(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 1)
		adapter := &idleTestAdapter{Adapter: run.current.dependencies.LLM}
		run.current.dependencies.LLM = adapter
		run.start(t)
		if len(adapter.idle) != 0 {
			t.Fatal("maintenance ran with a pending operation")
		}
		run.update(t, 0, operation.StatusCompleted)
		if len(run.calls) != 1 || len(adapter.idle) != 0 {
			t.Fatal("tool result must reach the model before maintenance")
		}
		run.respond(t, 0, textResponse("finished tools"))
		if len(adapter.idle) != 1 {
			t.Fatal("maintenance did not start after tools and model finished")
		}
		run.input(t, stopInput(t, "stop", inbox.StopWhenIdle))
		run.assertStopped(t)
	})
}

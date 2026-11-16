package coordinator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
)

// Clock advancement is not a happens-before edge. Inspect the adapter's locked
// request log, rather than racing a future timer's append to run.calls.
func (run *stopTestRun) modelCallCount() int {
	adapter := run.current.dependencies.LLM
	if idle, ok := adapter.(*idleTestAdapter); ok {
		adapter = idle.Adapter
	}
	return len(adapter.(*fakeAdapter).requestSnapshot())
}

func TestChildFinishReceiptsWakeImmediately(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 2)
		for i, op := range run.store.resume.Operations {
			spec, err := operation.NewRemoteJobSpec(operation.RemoteJobPlan{Type: "subagent", Version: 1, Data: []byte(`{"Action":"start"}`)})
			if err != nil {
				t.Fatal(err)
			}
			op.Type, op.Version, op.State, op.MaxOutputLength = spec.Type, spec.Version, spec.State, spec.MaxOutputLength
			run.store.resume.Operations[i] = op
			status := run.store.items[i+2].Data.(sessionstore.ToolCallStatus)
			status.Operations = []operation.Operation{op}
			run.store.items[i+2].Data = status
		}
		run.start(t)
		run.update(t, 0, operation.StatusCompleted)
		if run.modelCallCount() != 1 || len(run.store.appendedStatuses) != 1 {
			t.Fatal("Finish receipt did not wake the parent immediately and durably", run.modelCallCount())
		}
		if !run.current.hasPendingOperations() {
			t.Fatal("test lost the unfinished sibling")
		}
		assertCompletedResults(t, run.calls[0].request, 1)
		run.respond(t, 0, textResponse("reviewed"))
		run.input(t, stopInput(t, "stop", inbox.StopHard))
		run.update(t, 1, operation.StatusCanceled)
		run.assertStopped(t)
	})
}

func TestPeerReportsWakeImmediatelyAndSlurpTogether(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 0)
		run.start(t)
		run.input(t,
			progressPeerInput(t, "one", "ended", "first report"),
			progressPeerInput(t, "two", "ended", "second report"),
			progressPeerInput(t, "three", "ended", "third report"),
			progressPeerInput(t, "progress", "progress", "latest progress"),
		)
		if run.modelCallCount() != 1 || len(run.store.appendedInputs) != 4 {
			t.Fatal("reports did not wake immediately together or were not durable", run.modelCallCount())
		}
		request := requestDump(t, run.calls[0].request)
		for _, text := range []string{"first report", "second report", "third report", "latest progress"} {
			if strings.Count(request, text) != 1 {
				t.Fatalf("report missing or duplicated: %s", request)
			}
		}
		run.respond(t, 0, textResponse("reviewed"))
		if run.modelCallCount() != 1 || run.current.pendingInputs() != 0 {
			t.Fatal("delivered reports woke the parent again")
		}
		run.input(t, progressPeerInput(t, "four", "ended", "later report"))
		if run.modelCallCount() != 2 || !strings.Contains(requestDump(t, run.calls[1].request), "later report") {
			t.Fatal("later report did not wake immediately")
		}
		run.respond(t, 1, textResponse("reviewed again"))
		run.input(t, stopInput(t, "stop", inbox.StopWhenIdle))
		run.assertStopped(t)
	})
}

func TestPeerReportWakesBeforeWhenIdleStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 0)
		run.current.dependencies.RestoredStop = &inbox.ControlMessage{Mode: inbox.StopWhenIdle}
		run.current.dependencies.InitialInputs = []inbox.Input{progressPeerInput(t, "report", "ended", "pending report")}
		run.start(t)
		run.assertRunning(t)
		if run.modelCallCount() != 1 || !strings.Contains(requestDump(t, run.calls[0].request), "pending report") {
			t.Fatal("when-idle stop delayed or dropped the pending report")
		}
		run.respond(t, 0, textResponse("reviewed"))
		run.assertStopped(t)
	})
}

func TestPeerReportsSlurpWithOtherInputs(t *testing.T) {
	for _, kind := range []string{"user", "approval", "question", "message", "stop", "when-idle"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				run := newStopTestRun(t, 0)
				run.start(t)
				var input inbox.Input
				switch kind {
				case "user":
					input = externalEvent(t, 0, "user", "review now")
				case "stop":
					input = stopInput(t, "stop", inbox.StopHard)
				case "when-idle":
					input = stopInput(t, "stop", inbox.StopWhenIdle)
				default:
					text := map[string]string{"approval": "Child child-op asks for approval of request request: Bash command. It waits until you answer.", "question": "Which file should I change? Waiting for your answer.", "message": "ordinary message without an urgency hint"}[kind]
					input = progressPeerInput(t, "other", "message", text)
				}
				run.input(t, progressPeerInput(t, "report", "ended", "completed report"), input)
				if len(run.store.appendedInputs) != 2 {
					t.Fatal("input was not durable")
				}
				if kind == "stop" {
					run.assertStopped(t)
					if run.modelCallCount() != 0 {
						t.Fatal("hard stop ran a model")
					}
					return
				}
				if run.modelCallCount() != 1 || !strings.Contains(requestDump(t, run.calls[0].request), "completed report") {
					t.Fatal("input waited or dropped the report", run.modelCallCount())
				}
				run.respond(t, 0, textResponse("handled"))
				if kind != "when-idle" {
					run.input(t, stopInput(t, "stop", inbox.StopWhenIdle))
				}
				run.assertStopped(t)
			})
		})
	}
}

func TestPeerReportsDuringModelWaitForNextTurn(t *testing.T) {
	for _, bridge := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			run := newStopTestRun(t, 0)
			run.start(t)
			run.input(t, externalEvent(t, 0, "task", "delegate"))
			if bridge {
				options := run.current.dependencies.LLM.(*fakeAdapter).requestOptionsSnapshot()[0]
				if err := options.BeginTools(run.calls[0].ctx, false); err != nil {
					t.Fatal(err)
				}
			}
			run.input(t, progressPeerInput(t, "one", "ended", "first report"))
			run.input(t, progressPeerInput(t, "two", "ended", "second report"), progressPeerInput(t, "question", "message", "child question"))
			if run.modelCallCount() != 1 || run.calls[0].ctx.Err() != nil {
				t.Fatal("active turn was interrupted")
			}
			run.respond(t, 0, textResponse("delegated"))
			if run.modelCallCount() != 2 {
				t.Fatal("pending reports were delayed after the active turn")
			}
			for _, text := range []string{"first report", "second report", "child question"} {
				if strings.Count(requestDump(t, run.calls[1].request), text) != 1 {
					t.Fatal("next turn lost or duplicated a report")
				}
			}
			run.respond(t, 1, textResponse("reviewed"))
			run.input(t, stopInput(t, "stop", inbox.StopWhenIdle))
			run.assertStopped(t)
		})
	}
}

func TestPendingPeerReportsSurviveRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		directory := t.TempDir()
		store, err := localfile.New(directory)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Create(t.Context(), "session-1"); err != nil {
			t.Fatal(err)
		}
		run := newStopTestRun(t, 0)
		restoreTestRun(t, run, store)
		ctx, cancel := context.WithCancel(t.Context())
		go func() { run.done <- run.run(ctx) }()
		synctest.Wait()
		run.input(t, externalEvent(t, 0, "task", "delegate"))
		run.input(t, progressPeerInput(t, "one", "ended", "durable first report"), progressPeerInput(t, "two", "ended", "durable second report"))
		if run.modelCallCount() != 1 || run.current.pendingInputs() != 3 {
			t.Fatal("reports did not wait for the active turn")
		}
		cancel()
		synctest.Wait()
		if err := <-run.done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		reopened, err := localfile.New(directory)
		if err != nil {
			t.Fatal(err)
		}
		resumed := newStopTestRun(t, 0)
		restoreTestRun(t, resumed, reopened)
		resumed.start(t)
		if resumed.modelCallCount() != 1 {
			t.Fatal("restart did not deliver the pending reports together")
		}
		for _, text := range []string{"durable first report", "durable second report"} {
			if strings.Count(requestDump(t, resumed.calls[0].request), text) != 1 {
				t.Fatal("restart lost or duplicated a report")
			}
		}
		resumed.respond(t, 0, textResponse("reviewed"))
		if resumed.current.pendingInputs() != 0 {
			t.Fatal("restored reports did not become delivered")
		}
		resumed.input(t, stopInput(t, "stop", inbox.StopWhenIdle))
		resumed.assertStopped(t)
	})
}

func TestPeerReportImmediatelyInterruptsIdleMaintenance(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 0)
		adapter := &idleTestAdapter{Adapter: run.current.dependencies.LLM}
		run.current.dependencies.LLM = adapter
		run.start(t)
		run.input(t, externalEvent(t, 0, "task", "delegate"))
		run.respond(t, 0, textResponse("delegated"))
		run.input(t, progressPeerInput(t, "report", "ended", "completed report"))
		if run.modelCallCount() != 2 || adapter.idle[0].Err() == nil {
			t.Fatal("completion report did not interrupt maintenance immediately")
		}
		run.respond(t, 1, textResponse("reviewed"))
		run.input(t, stopInput(t, "stop", inbox.StopWhenIdle))
		run.assertStopped(t)
	})
}

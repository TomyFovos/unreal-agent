package coordinator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

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

func TestChildFinishReceiptsShareThePeerReportBatch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 3)
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
		// The receipt is durable after reconciliation, rather than at the
		// earlier Operation update. Its batch deadline starts there.
		deadline := time.Now().Add(45 * time.Second)
		synctest.Sleep(20 * time.Second)
		run.update(t, 1, operation.StatusCompleted)
		run.input(t, progressPeerInput(t, "ended", "ended", "peer report"))
		if run.modelCallCount() != 0 || len(run.store.appendedInputs) != 1 || len(run.store.appendedStatuses) != 2 {
			t.Fatal("Finish receipts did not wait durably with peer reports", run.modelCallCount())
		}
		synctest.Sleep(time.Until(deadline.Add(3 * slurpIdleTimeout)))
		synctest.Wait()
		if run.modelCallCount() != 1 {
			t.Fatal("Finish receipt batch missed its deadline")
		}
		if !run.current.hasPendingOperations() {
			t.Fatal("test lost the unfinished sibling before the deadline")
		}
		assertCompletedResults(t, run.calls[0].request, 2)
		if strings.Count(requestDump(t, run.calls[0].request), "peer report") != 1 {
			t.Fatal("peer report did not share the Finish receipt turn")
		}
		run.respond(t, 0, textResponse("reviewed"))
		run.input(t, stopInput(t, "stop", inbox.StopHard))
		run.update(t, 2, operation.StatusCanceled)
		run.assertStopped(t)
	})
}

func TestPeerReportsBatchAtFirstDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 0)
		run.start(t)
		deadline := time.Now().Add(45 * time.Second)
		run.input(t, progressPeerInput(t, "one", "ended", "first report"))
		synctest.Sleep(20 * time.Second)
		run.input(t, progressPeerInput(t, "two", "ended", "second report"))
		synctest.Sleep(23 * time.Second)
		run.input(t, progressPeerInput(t, "three", "ended", "third report"), progressPeerInput(t, "progress", "progress", "latest progress"))
		synctest.Sleep(time.Until(deadline.Add(-time.Millisecond)))
		synctest.Wait()
		if run.modelCallCount() != 0 || run.current.pendingInputs() != 4 || len(run.store.appendedInputs) != 4 {
			t.Fatal("reports woke early or were not durable")
		}
		synctest.Sleep(3 * slurpIdleTimeout)
		synctest.Wait()
		if run.modelCallCount() != 1 || run.current.peerBatch != nil {
			t.Fatal("first report's 45 second deadline was extended", run.modelCallCount())
		}
		request := requestDump(t, run.calls[0].request)
		for _, text := range []string{"first report", "second report", "third report", "latest progress"} {
			if strings.Count(request, text) != 1 {
				t.Fatalf("report missing or duplicated: %s", request)
			}
		}
		run.respond(t, 0, textResponse("reviewed"))
		synctest.Sleep(45 * time.Second)
		synctest.Wait()
		if run.modelCallCount() != 1 || run.current.pendingInputs() != 0 {
			t.Fatal("a drained batch woke the parent again")
		}
		run.input(t, stopInput(t, "stop", inbox.StopWhenIdle))
		run.assertStopped(t)
	})
}

func TestPeerBatchKeepsWhenIdleStopWaitingForDelivery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 0)
		run.current.dependencies.RestoredStop = &inbox.ControlMessage{Mode: inbox.StopWhenIdle}
		run.current.dependencies.InitialInputs = []inbox.Input{progressPeerInput(t, "report", "ended", "pending report")}
		run.start(t)
		run.assertRunning(t)
		if run.modelCallCount() != 0 {
			t.Fatal("report bypassed the batch")
		}
		synctest.Sleep(45 * time.Second)
		synctest.Wait()
		if run.modelCallCount() != 1 || !strings.Contains(requestDump(t, run.calls[0].request), "pending report") {
			t.Fatal("when-idle stop dropped the pending batch")
		}
		run.respond(t, 0, textResponse("reviewed"))
		run.assertStopped(t)
	})
}

func TestPeerBatchUrgentInputsWakeImmediately(t *testing.T) {
	for _, urgent := range []string{"user", "approval", "question", "message", "stop", "when-idle"} {
		t.Run(urgent, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				run := newStopTestRun(t, 0)
				run.start(t)
				run.input(t, progressPeerInput(t, "report", "ended", "queued report"))
				if run.modelCallCount() != 0 {
					t.Fatal("report did not wait")
				}
				switch urgent {
				case "user":
					run.input(t, externalEvent(t, 0, "user", "review now"))
				case "stop":
					run.input(t, stopInput(t, "stop", inbox.StopHard))
				case "when-idle":
					run.input(t, stopInput(t, "stop", inbox.StopWhenIdle))
				default:
					text := map[string]string{"approval": "Child child-op asks for approval of request request: Bash command. It waits until you answer.", "question": "Which file should I change? Waiting for your answer.", "message": "ordinary message without an urgency hint"}[urgent]
					run.input(t, progressPeerInput(t, "urgent", "message", text))
				}
				if run.current.peerBatch != nil {
					t.Fatal("urgent input left the batch timer running")
				}
				if urgent == "stop" {
					run.assertStopped(t)
					if run.modelCallCount() != 0 || len(run.store.appendedInputs) != 2 {
						t.Fatal("hard stop ran a model or lost durable input")
					}
					return
				}
				if run.modelCallCount() != 1 || !strings.Contains(requestDump(t, run.calls[0].request), "queued report") {
					t.Fatal("urgent input waited or dropped the queued report", run.modelCallCount())
				}
				run.respond(t, 0, textResponse("handled"))
				if urgent != "when-idle" {
					synctest.Sleep(45 * time.Second)
					synctest.Wait()
					if run.modelCallCount() != 1 {
						t.Fatal("canceled batch timer started another turn")
					}
					run.input(t, stopInput(t, "stop", inbox.StopWhenIdle))
				}
				run.assertStopped(t)
			})
		})
	}
}

func TestPeerReportsDuringModelWaitForNextTurnWithoutBatchDelay(t *testing.T) {
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
			if run.modelCallCount() != 1 || run.calls[0].ctx.Err() != nil || run.current.peerBatch != nil {
				t.Fatal("active turn was interrupted or a new report timer was started")
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

func TestPeerBatchPendingReportsSurviveRestart(t *testing.T) {
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
		synctest.Sleep(2 * slurpIdleTimeout)
		run.input(t, progressPeerInput(t, "one", "ended", "durable first report"), progressPeerInput(t, "two", "ended", "durable second report"))
		if run.modelCallCount() != 0 || run.current.batchedReports != 2 {
			t.Fatal("reports were not waiting as one batch")
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

func TestPeerBatchDeadlineInterruptsIdleMaintenance(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 0)
		adapter := &idleTestAdapter{Adapter: run.current.dependencies.LLM}
		run.current.dependencies.LLM = adapter
		run.start(t)
		run.input(t, externalEvent(t, 0, "task", "delegate"))
		run.respond(t, 0, textResponse("delegated"))
		run.input(t, progressPeerInput(t, "report", "ended", "completed report"))
		if run.modelCallCount() != 1 || adapter.idle[0].Err() != nil {
			t.Fatal("report interrupted maintenance before its deadline")
		}
		synctest.Sleep(45 * time.Second)
		synctest.Wait()
		if run.modelCallCount() != 2 || adapter.idle[0].Err() == nil {
			t.Fatal("maintenance delayed the report past its deadline")
		}
		run.respond(t, 1, textResponse("reviewed"))
		run.input(t, stopInput(t, "stop", inbox.StopWhenIdle))
		run.assertStopped(t)
	})
}

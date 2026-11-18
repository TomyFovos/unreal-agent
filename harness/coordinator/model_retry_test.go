package coordinator

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/claudeprompt"
	"github.com/unreallabsai/unreal-agent/harness/llm/responsesapi"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

type retryableTestError struct{}

func (retryableTestError) Error() string               { return "connection_error" }
func (retryableTestError) RetryableModelFailure() bool { return true }

func TestRetryableModelFailureKeepsSessionAndDoesNotReplayTools(t *testing.T) {
	for _, steer := range []bool{false, true} {
		t.Run(map[bool]string{true: "user_steers", false: "recovers"}[steer], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				run := newStopTestRun(t, 1)
				run.current.dependencies.RetryModelFailures = true
				next := run.current.dependencies.LLM
				var attempts atomic.Int32
				var records []llm.ActivityRecord
				var failedRequest llm.Request
				run.current.dependencies.RecordActivity = func(_ context.Context, r llm.ActivityRecord) error { records = append(records, r); return nil }
				run.current.dependencies.LLM = adapterWithOptions(func(ctx context.Context, req llm.Request, opt llm.RequestOptions) (llm.Response, error) {
					if attempts.Add(1) == 1 {
						failedRequest = req
						return llm.Response{}, retryableTestError{}
					}
					return next.Respond(ctx, req, opt)
				})
				run.start(t)
				run.update(t, 0, operation.StatusCompleted)
				run.assertRunning(t)
				if len(records) != 1 || records[0].Kind != "model_retry" || attempts.Load() != 1 {
					t.Fatal(records, attempts.Load())
				}
				if steer {
					run.input(t, externalEvent(t, 1, "steer", "answer this now"))
				} else {
					synctest.Sleep(2 * time.Second)
					synctest.Wait()
					synctest.Sleep(2 * slurpIdleTimeout)
					synctest.Wait()
				}
				if attempts.Load() != 2 || len(run.calls) != 1 || len(run.operations.adds) != 1 {
					t.Fatal("retry replayed operations or failed to resume", attempts.Load(), len(run.calls), run.operations.adds)
				}
				if !steer && !reflect.DeepEqual(failedRequest, run.calls[0].request) {
					t.Fatal("request changed across transport retry")
				}
				assertStopResult(t, run.calls[0].request, "call-0", string(operation.StatusCompleted))
				run.respond(t, 0, textResponse("recovered"))
				run.input(t, stopInput(t, "stop", inbox.StopHard))
				run.assertStopped(t)
			})
		})
	}
}

type adapterWithOptions func(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error)

func (a adapterWithOptions) Respond(c context.Context, r llm.Request, o llm.RequestOptions) (llm.Response, error) {
	return a(c, r, o)
}

func TestPermanentModelFailureStillReportsError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 0)
		failure := errors.New("permanent failure")
		run.current.dependencies.LLM = adapterWithOptions(func(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
			return llm.Response{}, failure
		})
		run.start(t)
		run.input(t, externalEvent(t, 0, "task", "hello"))
		select {
		case err := <-run.done:
			if !errors.Is(err, failure) {
				t.Fatal(err)
			}
		default:
			t.Fatal("permanent failure hidden")
		}
	})
}

func TestOutputLimitErrorRecordsObservedClaudeOutput(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 0)
		const text = "capped Claude output"
		failure := errors.New("Claude Code reached its output limit")
		response := llm.Response{ID: "claude-limited", Stop: llm.StopMaxOutputTokens,
			Failure: &llm.Failure{Code: "turn_failed", Message: failure.Error()},
			Output:  []llm.Item{{Type: llm.ItemMessage, OutputLimit: true, Data: llm.Message{Role: llm.RoleAssistant, Text: text}}},
		}
		run.current.dependencies.LLM = adapterWithOptions(func(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
			return response, failure
		})
		run.start(t)
		run.input(t, externalEvent(t, 0, "task", "hello"))
		select {
		case err := <-run.done:
			if !errors.Is(err, failure) {
				t.Fatal(err)
			}
		default:
			t.Fatal("terminal failure was lost")
		}
		got := run.store.appendedResponses
		if len(got) != 1 || !got[0].AdapterError || !reflect.DeepEqual(got[0].Response, response) {
			t.Fatalf("capped Claude response was lost: %+v", got)
		}
		projected, err := run.current.dependencies.ContextBuilder.Build()
		if err != nil || strings.Contains(claudeprompt.Plan(projected.Request.Input).Full, text) {
			t.Fatalf("capped Claude output entered the projection: %v", err)
		}
	})
}

func TestMainKeepsConfiguredProviderRetryPolicy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 0)
		attempts := 0
		run.current.dependencies.LLM = adapterWithOptions(func(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
			attempts++
			return llm.Response{}, retryableTestError{}
		})
		run.start(t)
		run.input(t, externalEvent(t, 0, "task", "hello"))
		select {
		case err := <-run.done:
			var temporary retryableTestError
			if !errors.As(err, &temporary) || attempts != 1 {
				t.Fatal(err, attempts)
			}
		default:
			t.Fatal("main exceeded the configured provider retry policy")
		}
	})
}

func TestFailedParentKeepsToolBoundaryInputsForNextUserTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 0)
		store := persistTestRun(t, run)
		restoreTestRun(t, run, store)
		run.current.dependencies.ContextBuilder = appendOnlyTestBuilder(t)
		run.current.dependencies.ContextBuilder.(interface{ SetRuntimeProvider(string) }).SetRuntimeProvider("claude")
		run.current.dependencies.RecordActivity = func(ctx context.Context, record llm.ActivityRecord) error {
			return store.AppendHostRecord(ctx, "session-1", sessionstore.HostRecord{Version: 1, Kind: sessionstore.HostActivity, Activity: &record})
		}
		model := run.current.dependencies.LLM.(*fakeAdapter)
		release := make(chan struct{})
		var attempts atomic.Int32
		model.respond = func(ctx context.Context, _ llm.Request) (llm.Response, error) {
			if attempts.Add(1) == 1 {
				select {
				case <-release:
					return llm.Response{}, errors.New("failed after tool boundary")
				case <-ctx.Done():
					return llm.Response{}, ctx.Err()
				}
			}
			return textResponse("reviewed"), nil
		}
		run.start(t)
		startModelFailureTestChild(t, run)
		run.input(t, externalEvent(t, 0, "task", "review the patch"))
		if run.modelCallCount() != 1 {
			t.Fatal("initial request did not start", run.modelCallCount())
		}
		const question = "Which file should I edit?"
		run.input(t, progressPeerInput(t, "question", "message", question))
		opt := model.requestOptionsSnapshot()[0]
		batch, err := opt.Steering(t.Context())
		if err != nil || len(batch) != 1 || !strings.Contains(batch[0].Text, question) {
			t.Fatal("tool boundary missed the child message", batch, err)
		}
		close(release)
		synctest.Wait()
		run.input(t, progressPeerInput(t, "progress", "progress", "still working"))
		run.assertRunning(t)
		if run.modelCallCount() != 1 || run.current.state.deliveredInputs != 0 || len(run.operations.cancels) != 0 {
			t.Fatal("failure retried, acknowledged inputs, or canceled the child")
		}
		run.input(t, externalEvent(t, 1, "next", "answer the question"))
		if run.modelCallCount() != 2 {
			t.Fatal("user input did not wake parent once", run.modelCallCount())
		}
		plan := claudeprompt.Plan(model.requestSnapshot()[1].Input)
		if strings.Count(plan.New, question) != 1 || !strings.Contains(plan.New, "still working") || !strings.Contains(plan.New, "answer the question") || run.current.pendingInputs() != 0 {
			t.Fatal("next turn lost or repeated inbox inputs", plan)
		}
		run.input(t, stopInput(t, "stop", inbox.StopHard))
		finishModelFailureTestChild(t, run)
		run.assertStopped(t)
	})
}

func TestParentContextSummaryFailureKeepsChild(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 0)
		store := persistTestRun(t, run)
		previous := run.store.items[0].Data.(session.Turn).ID
		for i := range 10 {
			id := session.TurnID(fmt.Sprintf("old-%d", i))
			if err := store.AppendInput(t.Context(), "session-1", externalEvent(t, i, "input-"+inbox.ID(id), strings.Repeat("older conversation ", 4000))); err != nil {
				t.Fatal(err)
			}
			if err := store.AppendTurn(t.Context(), "session-1", session.Turn{ID: id, PreviousTurnID: previous, Type: session.TurnRegular}); err != nil {
				t.Fatal(err)
			}
			if err := store.AppendModelResponse(t.Context(), "session-1", sessionstore.ModelResponse{TurnID: id, Response: textResponse("Earlier answer.")}); err != nil {
				t.Fatal(err)
			}
			previous = id
		}
		restoreTestRun(t, run, store)
		run.current.dependencies.ContextBuilder = appendOnlyTestBuilder(t)
		var activities []llm.ActivityRecord
		run.current.dependencies.RecordActivity = func(_ context.Context, record llm.ActivityRecord) error {
			activities = append(activities, record)
			return nil
		}
		model := run.current.dependencies.LLM.(*fakeAdapter)
		var calls atomic.Int32
		model.respond = func(context.Context, llm.Request) (llm.Response, error) {
			switch calls.Add(1) {
			case 1:
				return llm.Response{}, &responsesapi.APIError{Code: "context_length_exceeded"}
			case 2:
				return llm.Response{}, errors.New("summary endpoint unavailable")
			default:
				return textResponse("continued"), nil
			}
		}
		run.start(t)
		startModelFailureTestChild(t, run)
		run.input(t, externalEvent(t, 20, "latest", "Continue"))
		run.assertRunning(t)
		if calls.Load() != 2 || len(activities) != 1 || activities[0].Kind != llm.ActivityModelFailure || !strings.Contains(activities[0].Reason, "summary endpoint unavailable") || !run.current.hasPendingOperations() {
			t.Fatal("overflow recovery retried or terminated the Session after the summary failed", calls.Load(), activities)
		}
		run.input(t, progressPeerInput(t, "reply", "message", "child has a result"))
		if calls.Load() != 3 || !strings.Contains(requestDump(t, model.requestSnapshot()[2]), "child has a result") {
			t.Fatal("summary failure blocked the child's next message", calls.Load())
		}
		run.input(t, stopInput(t, "stop", inbox.StopHard))
		finishModelFailureTestChild(t, run)
		run.assertStopped(t)
	})
}

func startModelFailureTestChild(t *testing.T, run *stopTestRun) {
	t.Helper()
	spec, err := operation.NewRemoteJobSpec(operation.RemoteJobPlan{Type: "subagent", Version: 1, Data: []byte(`{"Action":"start"}`)})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(spec)
	payload, _ := json.Marshal(inbox.ControlMessage{Mode: inbox.DispatchOperation, Parameters: inbox.OperationIntent{OperationID: "child-op", ToolName: "SubagentStart", Spec: raw}})
	run.input(t, inbox.Input{ID: "start", Kind: inbox.InputControl, Payload: payload})
}

func finishModelFailureTestChild(t *testing.T, run *stopTestRun) {
	t.Helper()
	canceled, err := operation.CancelUndispatched(run.operations.adds[0])
	if err != nil {
		t.Fatal(err)
	}
	run.operations.updates <- canceled
	synctest.Wait()
	synctest.Sleep(2 * slurpIdleTimeout)
	synctest.Wait()
}

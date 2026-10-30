package coordinator

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
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

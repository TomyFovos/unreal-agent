package coordinator

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/responsesapi"
	"github.com/unreallabsai/unreal-agent/harness/primitives"
)

func TestUsageLimitWaitRetriesAtFiveMinutesOrEarlierReset(t *testing.T) {
	for _, child := range []bool{false, true} {
		for _, hinted := range []bool{false, true} {
			t.Run(fmt.Sprintf("child=%t/hint=%t", child, hinted), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					run := newStopTestRun(t, 0)
					run.current.dependencies.RetryModelFailures = child
					delay := primitives.DefaultRemoteRequest("", "", "").RetryPolicy.MaxBackoff
					reset := time.Time{}
					updated := time.Time{}
					if hinted {
						delay = 5 * time.Minute
						reset = time.Now().Add(2 * time.Hour)
						updated = reset.Add(time.Hour)
					}
					called := make(chan time.Time, 3)
					states := make(chan time.Time, 4)
					updates := make(chan time.Time, 4)
					ready := make(chan struct{}, 1)
					finish := make(chan llm.Response)
					run.current.dependencies.ModelWaiting = func(_ context.Context, at time.Time) error { states <- at; return nil }
					run.current.dependencies.ModelWaitUpdated = func(at time.Time) { updates <- at }
					var attempts atomic.Int32
					run.current.dependencies.LLM = adapterWithOptions(func(ctx context.Context, _ llm.Request, _ llm.RequestOptions) (llm.Response, error) {
						attempt := attempts.Add(1)
						called <- time.Now()
						if attempt == 1 {
							return llm.Response{}, &responsesapi.APIError{StatusCode: 429, Type: "usage_limit_reached", ResetAt: reset}
						}
						if attempt == 2 && hinted {
							return llm.Response{}, &responsesapi.APIError{Type: "usage_limit_reached", ResetAt: updated}
						}
						ready <- struct{}{}
						select {
						case response := <-finish:
							return response, nil
						case <-ctx.Done():
							return llm.Response{}, ctx.Err()
						}
					})
					run.start(t)
					run.input(t, externalEvent(t, 0, "task", "work"))
					until, firstAt := <-states, <-called
					if len(states) != 0 || (!hinted && until.Sub(firstAt) != delay || hinted && !until.Equal(reset)) || attempts.Load() != 1 {
						t.Fatal(until, attempts.Load())
					}
					run.input(t, progressPeerInput(t, "peer", "message", "parent message"))
					requireRetry := func(previous time.Time, wantAttempts int32) time.Time {
						t.Helper()
						synctest.Sleep(time.Until(previous.Add(delay)) - time.Nanosecond)
						synctest.Wait()
						if attempts.Load() != wantAttempts-1 {
							t.Fatal("retried before the scheduled time", attempts.Load())
						}
						synctest.Sleep(time.Nanosecond + 2*slurpIdleTimeout)
						synctest.Wait()
						if attempts.Load() != wantAttempts || len(states) != 0 {
							t.Fatal("retry did not keep the same wait", attempts.Load(), len(states))
						}
						return <-called
					}
					secondAt := requireRetry(firstAt, 2)
					if hinted {
						if len(updates) != 1 || !(<-updates).Equal(updated) || len(ready) != 0 {
							t.Fatal("the first early rejection did not update the same wait")
						}
						requireRetry(secondAt, 3)
					}
					if len(ready) != 1 {
						t.Fatal("the successful retry did not start")
					}
					<-ready
					finish <- textResponse("resumed")
					synctest.Wait()
					synctest.Sleep(2 * slurpIdleTimeout)
					synctest.Wait()
					if len(states) != 1 || !(<-states).IsZero() {
						t.Fatal("successful retry did not close exactly one wait", len(states))
					}
					run.input(t, stopInput(t, "stop", inbox.StopHard))
					run.assertStopped(t)
				})
			})
		}
	}
}

func TestUsageLimitWaitStopAndInterruptAreImmediate(t *testing.T) {
	for _, mode := range []inbox.ControlMode{inbox.StopHard, inbox.InterruptAgent} {
		t.Run(string(mode), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				run := newStopTestRun(t, 0)
				var attempts atomic.Int32
				states := make(chan time.Time, 4)
				run.current.dependencies.ModelWaiting = func(_ context.Context, at time.Time) error { states <- at; return nil }
				run.current.dependencies.LLM = adapterWithOptions(func(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
					attempts.Add(1)
					return llm.Response{}, &responsesapi.APIError{Type: "usage_limit_reached", ResetAt: time.Now().Add(43 * time.Hour)}
				})
				run.start(t)
				run.input(t, externalEvent(t, 0, "task", "work"))
				if until := <-states; !until.After(time.Now()) {
					t.Fatal("missing usage wait", until)
				}
				before := time.Now()
				run.input(t, stopInput(t, "interrupt", mode))
				if waiting := <-states; time.Since(before) > 2*slurpIdleTimeout || !waiting.IsZero() {
					t.Fatal("stop waited for reset", waiting)
				}
				if mode == inbox.InterruptAgent {
					run.input(t, stopInput(t, "stop", inbox.StopHard))
				}
				run.assertStopped(t)
				synctest.Sleep(44 * time.Hour)
				synctest.Wait()
				if attempts.Load() != 1 {
					t.Fatal("retried after stop", attempts.Load())
				}
			})
		})
	}
}

func TestUsageLimitWithoutHintUsesLongestBackoffIndefinitely(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 0)
		next := run.current.dependencies.LLM
		var mu sync.Mutex
		var attempts []time.Time
		snapshot := func() []time.Time {
			mu.Lock()
			defer mu.Unlock()
			return append([]time.Time(nil), attempts...)
		}
		var limited atomic.Bool
		limited.Store(true)
		deadlines := make(chan time.Time, 1)
		run.current.dependencies.ModelWaiting = func(_ context.Context, at time.Time) error {
			if !at.IsZero() {
				deadlines <- at
			}
			return nil
		}
		run.current.dependencies.ModelWaitUpdated = func(at time.Time) { deadlines <- at }
		run.current.dependencies.LLM = adapterWithOptions(func(ctx context.Context, req llm.Request, opt llm.RequestOptions) (llm.Response, error) {
			mu.Lock()
			attempts = append(attempts, time.Now())
			mu.Unlock()
			if limited.Load() {
				return llm.Response{}, &responsesapi.APIError{Type: "usage_limit_reached"}
			}
			return next.Respond(ctx, req, opt)
		})
		run.start(t)
		run.input(t, externalEvent(t, 0, "task", "work"))
		backoff := primitives.DefaultRemoteRequest("", "", "").RetryPolicy.MaxBackoff
		for i := range int(2 * time.Hour / backoff) {
			synctest.Sleep(time.Until(<-deadlines) - time.Nanosecond)
			synctest.Wait()
			if got := len(snapshot()); got != i+1 {
				t.Fatal("retried before backoff", got, i+1)
			}
			synctest.Sleep(time.Nanosecond + 2*slurpIdleTimeout)
			synctest.Wait()
			if got := len(snapshot()); got != i+2 {
				t.Fatal("retry stopped", got, i+2)
			}
		}
		observed := snapshot()
		for i := 1; i < len(observed); i++ {
			if observed[i].Sub(observed[i-1]) < backoff {
				t.Fatal("tight retry", observed[i].Sub(observed[i-1]))
			}
		}
		limited.Store(false)
		synctest.Sleep(backoff + 2*slurpIdleTimeout)
		synctest.Wait()
		if len(run.calls) != 1 {
			t.Fatal("did not resume automatically")
		}
		run.respond(t, 0, textResponse("resumed"))
		run.input(t, stopInput(t, "stop", inbox.StopHard))
		run.assertStopped(t)
	})
}

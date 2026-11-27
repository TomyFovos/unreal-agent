package host

import (
	"context"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/responsesapi"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func TestUsageLimitWaitingIsObservableAndStopClearsIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		until := time.Now().Add(43 * time.Hour)
		calls := 0
		h, err := New(t.Context(), Config{Directory: t.TempDir(), Build: factory(modelFunc(func(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
			calls++
			return llm.Response{}, &responsesapi.APIError{Type: "usage_limit_reached", ResetAt: until}
		}))})
		if err != nil {
			t.Fatal(err)
		}
		defer h.Close()
		s, err := h.Create(t.Context(), Options{ID: "wait", Initial: []inbox.Input{input("task", "work")}})
		if err != nil {
			t.Fatal(err)
		}
		synctest.Sleep(10 * time.Millisecond)
		synctest.Wait()
		v, err := s.Inspect(0, 256)
		if err != nil || v.Progress == nil || v.Progress.Mode != "usage_limit_wait" || !v.Progress.RetryAt.Equal(until) {
			t.Fatal("TUI snapshot", v.Progress, err)
		}
		if busy, _ := s.ModelBusy(); busy {
			t.Fatal("waiting looks like inference")
		}
		var records int
		for _, item := range v.History.Items {
			if r, ok := item.Data.(sessionstore.HostRecord); ok && r.Activity != nil && r.Activity.Kind == "usage_limit_wait" && r.Activity.RetryAt.Equal(until) {
				records++
			}
		}
		if records != 1 {
			t.Fatal("missing canonical waiting state", records)
		}
		synctest.Sleep(5*time.Minute - 11*time.Millisecond)
		synctest.Wait()
		if calls != 1 {
			t.Fatal("wait retried before five minutes", calls)
		}
		if _, err := s.Stop(t.Context(), s.Generation, inbox.StopHard, "user stop"); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if err := s.Wait(t.Context()); err != nil {
			t.Fatal(err)
		}
		v, err = s.Inspect(0, 256)
		if err != nil || v.Progress != nil || v.Running {
			t.Fatal("stop left a wait", v.Progress, err)
		}
		synctest.Sleep(time.Minute)
		synctest.Wait()
		if calls != 1 {
			t.Fatal("stopped wait retried", calls)
		}
	})
}

func TestUsageLimitWaitRetriesOnUserInputOrResume(t *testing.T) {
	for _, action := range []string{"message", "resume"} {
		t.Run(action, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				until := time.Now().Add(2 * time.Hour)
				updated := time.Now().Add(time.Minute)
				requests := make(chan llm.Request, 4)
				reject := make(chan struct{})
				calls := 0
				model := modelFunc(func(ctx context.Context, r llm.Request, opt llm.RequestOptions) (llm.Response, error) {
					calls++
					requests <- r
					switch calls {
					case 1:
						return llm.Response{}, &responsesapi.APIError{Type: "usage_limit_reached", ResetAt: until}
					case 2:
						select {
						case <-reject:
						case <-ctx.Done():
							return llm.Response{}, ctx.Err()
						}
						return llm.Response{}, &responsesapi.APIError{Type: "usage_limit_reached", ResetAt: updated}
					default:
						return echo(ctx, r, opt)
					}
				})
				h, err := New(t.Context(), Config{Directory: t.TempDir(), Build: factory(model)})
				if err != nil {
					t.Fatal(err)
				}
				defer h.Close()
				s, err := h.Create(t.Context(), Options{ID: "wait", Initial: []inbox.Input{input("task", "work")}})
				if err != nil {
					t.Fatal(err)
				}
				check := func(retryAt time.Time, wantRecords int) {
					t.Helper()
					synctest.Wait()
					v, err := s.Inspect(0, 256)
					if err != nil || !v.Progress.UsageRetryAt().Equal(retryAt) {
						t.Fatalf("TUI wait = %+v, want %v (%v)", v.Progress, retryAt, err)
					}
					records := 0
					for _, item := range v.History.Items {
						if r, ok := item.Data.(sessionstore.HostRecord); ok && r.Activity != nil && r.Activity.Kind == "usage_limit_wait" {
							records++
						}
					}
					if records != wantRecords {
						t.Fatalf("wait records = %d, want %d", records, wantRecords)
					}
				}
				synctest.Sleep(10 * time.Millisecond)
				check(until, 1)
				if len(requests) != 1 {
					t.Fatal("first call did not enter the wait")
				}
				<-requests
				if action == "message" {
					if _, err = s.Submit(t.Context(), s.Generation, input("follow-up", "also check tests")); err != nil {
						t.Fatal(err)
					}
				} else if resumed, err := h.Resume(t.Context(), Options{ID: s.ID}); err != nil || resumed != s {
					t.Fatalf("resume did not reuse the running Session: %v", err)
				}
				synctest.Sleep(10 * time.Millisecond)
				check(until, 1) // Keep displaying the wait while the second call is in flight.
				if len(requests) != 1 {
					t.Fatalf("%s did not start exactly one immediate retry", action)
				}
				request := <-requests
				if action == "message" && !slices.ContainsFunc(request.Input, func(item llm.Item) bool {
					m, ok := item.Data.(llm.Message)
					return ok && m.Role == llm.RoleUser && m.Text == "also check tests"
				}) {
					t.Fatal("immediate retry lost the new user message")
				}
				close(reject)
				check(updated, 1)
				synctest.Sleep(time.Until(updated) - time.Nanosecond)
				synctest.Wait()
				if len(requests) != 0 {
					t.Fatal("retried before the updated reset")
				}
				synctest.Sleep(time.Nanosecond + 10*time.Millisecond)
				check(time.Time{}, 2)
				v, err := s.Inspect(0, 256)
				if err != nil || len(requests) != 1 || !slices.ContainsFunc(v.History.Items, func(item HistoryItem) bool {
					r, ok := item.Data.(sessionstore.ModelResponse)
					return ok && len(r.Response.Output) == 1 && r.Response.Output[0].Data.(llm.Message).Text == "done"
				}) {
					t.Fatal("turn did not finish after the updated reset", err)
				}
			})
		})
	}
}

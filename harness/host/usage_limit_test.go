package host

import (
	"context"
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
		synctest.Sleep(42 * time.Hour)
		synctest.Wait()
		if calls != 1 {
			t.Fatal("wait retried", calls)
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
	})
}

package host

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
)

type pendingOperationManager struct{ operation.Manager }

func (pendingOperationManager) Add(operation.Operation) error { return nil }

func TestHostRestartProtectsWorkInOtherSessions(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		release := make(chan struct{})
		build := factory(modelFunc(func(ctx context.Context, req llm.Request, opts llm.RequestOptions) (llm.Response, error) {
			select {
			case <-ctx.Done():
				return llm.Response{}, ctx.Err()
			case <-release:
				return echo(ctx, req, opts)
			}
		}))
		h, err := New(ctx, Config{Directory: t.TempDir(), Build: func(ctx context.Context, id session.ID) (Runtime, error) {
			runtime, err := build(ctx, id)
			runtime.Operations = pendingOperationManager{runtime.Operations}
			return runtime, err
		}})
		if err != nil {
			t.Fatal(err)
		}
		defer h.Close()
		if _, err := h.Create(ctx, Options{ID: "viewer"}); err != nil {
			t.Fatal(err)
		}
		busy, err := h.Create(ctx, Options{ID: "other"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := busy.Submit(ctx, busy.Generation, input("task", "work")); err != nil {
			t.Fatal(err)
		}
		synctest.Sleep(10 * time.Millisecond)
		synctest.Wait()
		if work, restart := h.PrepareRestart(ctx, nil); restart || work != (Workload{ActiveSessions: 1}) {
			t.Fatalf("restart during model turn: %+v, accepted=%t", work, restart)
		}

		close(release)
		spec, err := operation.NewRemoteJobSpec(operation.RemoteJobPlan{Type: "test", Version: 1, Data: []byte(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"SubagentStart", "OtherOperation"} {
			if _, err := busy.SubmitOperation(ctx, busy.Generation, inbox.ID("start-"+name), operation.ID(name), name, spec); err != nil {
				t.Fatal(err)
			}
		}
		synctest.Sleep(10 * time.Millisecond)
		synctest.Wait()
		if running, _ := busy.ModelBusy(); running {
			t.Fatal("model call still running")
		}
		want := Workload{ActiveSessions: 1, Children: 1, Operations: 1}
		if work, restart := h.PrepareRestart(ctx, nil); restart || work != want {
			t.Fatalf("restart during operations: %+v, accepted=%t", work, restart)
		}
		if work, restart := h.PrepareRestart(ctx, &Workload{ActiveSessions: 1, Children: 1}); restart || work != want {
			t.Fatalf("stale confirmation: %+v, accepted=%t", work, restart)
		}
		if work, restart := h.PrepareRestart(ctx, &want); !restart || work != want {
			t.Fatalf("confirmed restart: %+v, accepted=%t", work, restart)
		}
		if err := h.Close(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestHostIdleRestartSealsSubmissionAndOpen(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		h := newTestHost(t, t.TempDir())
		s, err := h.Create(ctx, Options{ID: "session"})
		if err != nil {
			t.Fatal(err)
		}
		synctest.Sleep(10 * time.Millisecond)
		synctest.Wait()
		if work := h.Workload(); work != (Workload{}) {
			t.Fatalf("idle Session counted as work: %+v", work)
		}

		// Model the gap between admission under the Session lock and delivery
		// to the coordinator. An idle observation predates this input.
		prompt := input("queued", "work")
		pending, fresh, err := s.register(prompt)
		if err != nil || !fresh {
			t.Fatal("register input", err)
		}
		idle := Workload{}
		if work, restart := h.PrepareRestart(ctx, &idle); restart || work != (Workload{ActiveSessions: 1}) {
			t.Fatalf("restart during input admission: %+v, accepted=%t", work, restart)
		}
		if err := s.inbox.Submit(ctx, pending.input); err != nil {
			t.Fatal(err)
		}
		synctest.Sleep(10 * time.Millisecond)
		synctest.Wait()
		if work, restart := h.PrepareRestart(ctx, nil); !restart || work != idle {
			t.Fatalf("idle restart: %+v, accepted=%t", work, restart)
		}
		if _, err := s.Submit(ctx, s.Generation, input("late", "work")); !errors.Is(err, ErrStopped) {
			t.Fatal("retiring Session accepted work", err)
		}
		if _, err := h.Create(ctx, Options{ID: "late"}); !errors.Is(err, ErrStopped) {
			t.Fatal("retiring Host accepted another Session", err)
		}
	})
}

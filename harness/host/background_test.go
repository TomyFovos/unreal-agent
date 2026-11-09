package host

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
)

func TestBackgroundWorkCancelsAndJoinsBeforeClosing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		owner := newBackgroundWork(t.Context())
		started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
		if !owner.start(func(ctx context.Context) {
			close(started)
			<-ctx.Done()
			close(canceled)
			<-release
		}) {
			t.Fatal("live owner refused background work")
		}
		<-started
		closed := make(chan struct{})
		go func() { owner.close(); close(closed) }()
		<-canceled
		synctest.Wait()
		select {
		case <-closed:
			t.Fatal("owner returned before work finished")
		default:
		}
		if owner.start(func(context.Context) { t.Error("work started during shutdown") }) {
			t.Fatal("closing owner admitted more work")
		}
		close(release)
		<-closed
		owner.close()
	})
}

func TestSessionBackgroundWorkUsesOwnerLifetime(t *testing.T) {
	for _, end := range []string{"host_close", "session_stop", "parent_cancel"} {
		t.Run(end, func(t *testing.T) {
			parent, cancel := context.WithCancel(t.Context())
			defer cancel()
			ended := make(chan struct{})
			runtimeClosed := make(chan struct{})
			build := func(ctx context.Context, id session.ID) (Runtime, error) {
				r, err := factory(modelFunc(echo))(ctx, id)
				r.Close = func() error {
					select {
					case <-ended:
					default:
						t.Error("runtime closed before background work ended")
					}
					close(runtimeClosed)
					return nil
				}
				return r, err
			}
			h, err := New(parent, Config{Directory: t.TempDir(), Build: build})
			if err != nil {
				t.Fatal(err)
			}
			defer h.Close()
			s, err := h.Create(t.Context(), Options{ID: "background"})
			if err != nil {
				t.Fatal(err)
			}
			started := make(chan context.Context, 1)
			var lifetime func(func(context.Context)) bool
			model := s.withProgress(idleModel{
				Adapter: modelFunc(func(_ context.Context, _ llm.Request, o llm.RequestOptions) (llm.Response, error) {
					lifetime = o.StartBackground
					if lifetime == nil || !lifetime(func(ctx context.Context) {
						started <- ctx
						<-ctx.Done()
						close(ended)
					}) {
						t.Fatal("Respond missing owner lifetime")
					}
					return llm.Response{}, nil
				}),
				idle: func(_ context.Context, _ llm.Request, o llm.RequestOptions) {
					if o.StartBackground == nil {
						t.Error("Idle missing owner lifetime")
					}
				},
			})
			request, stopRequest := context.WithCancel(t.Context())
			defer stopRequest()
			if _, err := model.Respond(request, llm.Request{}, llm.RequestOptions{}); err != nil {
				t.Fatal(err)
			}
			ctx := <-started
			model.(llm.IdleAdapter).Idle(request, llm.Request{}, llm.RequestOptions{})
			stopRequest()
			if ctx.Err() != nil {
				t.Fatal("request/Idle cancellation ended owner lifetime")
			}
			switch end {
			case "host_close":
				if err := h.Close(); err != nil {
					t.Fatal(err)
				}
			case "session_stop":
				if _, err := s.Stop(timeout(t), s.Generation, inbox.StopWhenIdle, "test"); err != nil {
					t.Fatal(err)
				}
				if err := s.Wait(timeout(t)); err != nil {
					t.Fatal(err)
				}
			case "parent_cancel":
				cancel()
				if err := s.Wait(timeout(t)); err != nil && !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			}
			if ctx.Err() == nil {
				t.Fatal("owner lifetime survived shutdown")
			}
			select {
			case <-runtimeClosed:
			default:
				t.Fatal("Session ended before runtime cleanup")
			}
			if lifetime(func(context.Context) { t.Error("late work started") }) {
				t.Fatal("finished Session admitted background work")
			}
		})
	}
}

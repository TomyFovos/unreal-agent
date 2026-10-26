package permission

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

func handoffController(t *testing.T) *Controller {
	t.Helper()
	return &Controller{ctx: t.Context(), mode: ReadOnly, revision: 1, lease: "synthetic-control", active: map[string]context.CancelFunc{}, changed: make(chan struct{})}
}

func TestPermissionHandoffRevokedBeforeSendDoesNotCallCallback(t *testing.T) {
	c := handoffController(t)
	if err := c.SetMode("synthetic-control", 1, ReadOnly, ""); err != nil {
		t.Fatal(err)
	}
	calls := 0
	err := c.WithCurrentRevision(t.Context(), 1, func() error { calls++; return nil })
	if calls != 0 || Failure(err) == nil {
		t.Fatal("accepted revocation allowed a new control handoff")
	}
}

func TestPermissionHandoffSendAndModeAcceptanceAreSerialized(t *testing.T) {
	c := handoffController(t)
	entered, release, sent, accepted := make(chan struct{}), make(chan struct{}), make(chan error, 1), make(chan error, 1)
	var sendCompleted atomic.Bool
	go func() {
		sent <- c.WithCurrentRevision(t.Context(), 1, func() error { close(entered); <-release; sendCompleted.Store(true); return nil })
	}()
	<-entered
	// Mutex blocking is not a durable block for testing/synctest. Test the
	// actual shared lock directly, then the concurrent mode-change ordering.
	if c.mu.TryLock() {
		c.mu.Unlock()
		close(release)
		<-sent
		t.Fatal("control send did not hold the revocation lock")
	}
	go func() {
		err := c.SetMode("synthetic-control", 1, ReadOnly, "")
		if !sendCompleted.Load() {
			err = errors.New("mode accepted before send completed")
		}
		accepted <- err
	}()
	close(release)
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	if err := <-accepted; err != nil {
		t.Fatal(err)
	}
	calls := 0
	if err := c.WithCurrentRevision(t.Context(), 1, func() error { calls++; return nil }); Failure(err) == nil || calls != 0 {
		t.Fatal("old revision survived accepted revocation")
	}
}

func TestPermissionHandoffCanceledAndSendFailureAreNotSuccess(t *testing.T) {
	c := handoffController(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	called := false
	if err := c.WithCurrentRevision(ctx, 1, func() error { called = true; return nil }); Failure(err) == nil || called {
		t.Fatal("canceled handoff ran")
	}
	lost := errors.New("synthetic partial pipe write")
	if err := c.WithCurrentRevision(t.Context(), 1, func() error { return lost }); !errors.Is(err, lost) {
		t.Fatal("send uncertainty was hidden")
	}
}

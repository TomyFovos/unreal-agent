package coordinator

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/operation"
)

// Unlike the baseline-compatible processEvents tests, this file directly tests
// the new entry used when Run's outer select already received one preparation.
// It is a real owner boundary test, not a second scheduler implementation.
func TestPreparationWakeupOuterEntryRetainsDurableBarrier(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newWakeupOwnerFixture(t)
		ctx, cancel := context.WithCancelCause(t.Context())
		defer cancel(errors.New("synthetic initial preparation cleanup"))
		p := operation.PreparedOperation{Operation: f.prepared, Acknowledge: func(err error) { f.acks <- err }}
		done := make(chan error, 1)
		startedAt := time.Now()
		go func() { _, err := f.current.processEventsWithPreparation(ctx, &p); done <- err }()
		synctest.Wait()
		select {
		case at := <-f.store.started:
			if !at.Equal(startedAt) {
				t.Fatal("outer-select preparation was sent back through intentional idle waits")
			}
		default:
			t.Error("outer preparation entry did not reach the real owner durable save")
			cancel(errors.New("synthetic initial preparation cleanup"))
			<-done
			return
		}
		if len(f.acks) != 0 || len(f.store.savedOperations) != f.baseSave {
			t.Fatal("outer entry ACK preceded durable release")
		}
		close(f.store.release)
		synctest.Wait()
		if len(f.acks) != 1 || <-f.acks != nil {
			t.Fatal("outer entry did not ACK exactly once after save")
		}
		cancel(errors.New("synthetic initial preparation assertions complete"))
		<-done
	})
}

func TestPreparationWakeupOuterEntryRespectsReadyHardStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newWakeupOwnerFixture(t)
		ctx, cancel := context.WithCancelCause(t.Context())
		defer cancel(errors.New("synthetic initial stop cleanup"))
		submitTestInput(t, f.current.dependencies.Inbox, stopInput(t, "synthetic-initial-hard-stop", inbox.StopHard))
		synctest.Wait()
		p := operation.PreparedOperation{Operation: f.prepared, Acknowledge: func(err error) { f.acks <- err }}
		done := make(chan error, 1)
		go func() { _, err := f.current.processEventsWithPreparation(ctx, &p); done <- err }()
		synctest.Wait()
		select {
		case err := <-f.acks:
			if !errors.Is(err, operation.ErrPreparationUncommitted) {
				t.Fatal("initial preparation overtook ready accepted StopHard", err)
			}
		default:
			t.Error("initial preparation was left waiting despite a ready hard-stop boundary")
		}
		if len(f.store.started) != 0 || f.current.stop.request.Mode != inbox.StopHard || len(f.store.appendedInputs) != 1 {
			t.Error("initial boundary lost the hard stop or started a forbidden preparation save")
		}
		cancel(errors.New("synthetic initial stop assertions complete"))
		<-done
	})
}

func TestPreparationWakeupClosedChannelRemainsDisabledAcrossOwnerRounds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newWakeupOwnerFixture(t)
		close(f.manager.prepared)
		for range 3 {
			at := time.Now()
			if _, err := f.current.processEvents(t.Context()); err != nil {
				t.Fatal(err)
			}
			if elapsed := time.Since(at); elapsed != 2*slurpIdleTimeout {
				t.Fatal("closed preparation channel spun or changed normal deadlines", elapsed)
			}
		}
		if len(f.acks) != 0 || len(f.store.started) != 0 {
			t.Fatal("closed channel yielded a phantom preparation")
		}
	})
}

// This case runs the actual outer Run select, rather than invoking its shared
// entry directly. The controlled worker cannot execute a native side effect;
// disk-backed Host/native execution is covered by the integration gate.
func TestPreparationWakeupActualRunOuterSelectSavesWithoutIdleWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newWakeupOwnerFixture(t)
		dependencies := f.current.dependencies
		dependencies.Sessions = wakeupSelectiveStore{f.store}
		dependencies.Restored.Operations = []operation.Operation{operation.WithExecutionBasis(f.prepared, nil)}
		f.current = New(dependencies).(*coordinator)
		ctx, cancel := context.WithCancelCause(t.Context())
		defer cancel(errors.New("synthetic outer Run cleanup"))
		f.submit(ctx)
		started := time.Now()
		done := make(chan error, 1)
		go func() { done <- f.current.Run(ctx) }()
		synctest.Wait()
		select {
		case at := <-f.store.started:
			if !at.Equal(started) {
				t.Fatal("outer Run inserted idle before preparation save", at.Sub(started))
			}
		default:
			t.Error("actual Run did not reach the blocked preparation save")
			cancel(errors.New("synthetic outer Run did not save"))
			<-done
			return
		}
		if len(f.acks) != 0 {
			t.Fatal("outer Run acknowledged an undurable checkpoint")
		}
		close(f.store.release)
		synctest.Wait()
		if len(f.acks) != 1 || <-f.acks != nil {
			t.Fatal("outer Run did not acknowledge its durable checkpoint exactly once")
		}
		cause := errors.New("synthetic completed outer Run test")
		cancel(cause)
		synctest.Wait()
		if err := <-done; !errors.Is(err, context.Canceled) && !errors.Is(err, cause) {
			t.Fatal("outer Run did not preserve its cancellation", err)
		}
	})
}

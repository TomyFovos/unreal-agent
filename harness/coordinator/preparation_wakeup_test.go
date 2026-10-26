package coordinator

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
)

// These tests exercise the real processEvents owner and commitOperationPreparation
// paths. Only the worker and durable Store are controlled fakes. A successful ACK
// is not represented as a native mutation here; native effects are covered by the
// disk-backed Host integration tests.
type wakeupPreparationManager struct {
	*fakeOperationManager
	prepared chan operation.PreparedOperation
}

func (m *wakeupPreparationManager) Preparations() <-chan operation.PreparedOperation {
	return m.prepared
}

type wakeupDurableStore struct {
	*fakeStore
	started chan time.Time
	release chan struct{}
}

func (s *wakeupDurableStore) SaveOperation(ctx context.Context, id session.ID, op operation.Operation) error {
	select {
	case s.started <- time.Now():
	default:
		panic("unexpected duplicate preparation save")
	}
	select {
	case <-s.release:
		return s.fakeStore.SaveOperation(ctx, id, op)
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

type wakeupOwnerFixture struct {
	current  *coordinator
	store    *wakeupDurableStore
	manager  *wakeupPreparationManager
	prepared operation.Operation
	acks     chan error
	baseSave int
}

func newWakeupOwnerFixture(t *testing.T) *wakeupOwnerFixture {
	t.Helper()
	f := newToolAdmissionFixture(t)
	manager := &wakeupPreparationManager{fakeOperationManager: f.manager, prepared: make(chan operation.PreparedOperation)}
	manager.updates = make(chan operation.Operation) // Match the real unbuffered owner channel.
	store := &wakeupDurableStore{fakeStore: f.store, started: make(chan time.Time, 1), release: make(chan struct{})}
	current := operation.WithPreparationRequired(operation.Operation{
		ID: "synthetic-wakeup", Type: operation.TypeValue, Version: operation.VersionValue,
		Status: operation.StatusReady, State: jsontext.Value(`{"synthetic":true}`),
	}, true)
	// The test begins at the already-accepted Operation boundary. It makes no
	// claim about native filesystem preparation or recovery-basis validation.
	f.current.addOperationToLocalState(current)
	f.current.dependencies.Operations = manager
	f.current.dependencies.Sessions = store
	return &wakeupOwnerFixture{current: f.current, store: store, manager: manager,
		prepared: operation.WithExecutionBasis(current, jsontext.Value(`{"synthetic_basis":true}`)),
		acks:     make(chan error, 2), baseSave: len(f.store.savedOperations)}
}

func (f *wakeupOwnerFixture) submit(ctx context.Context) {
	value := operation.PreparedOperation{Operation: f.prepared, Acknowledge: func(err error) { f.acks <- err }}
	go func() {
		select {
		case f.manager.prepared <- value:
		case <-ctx.Done():
		}
	}()
}

func TestPreparationWakeupReadyBeforeIdle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newWakeupOwnerFixture(t)
		ctx, cancel := context.WithCancelCause(t.Context())
		defer cancel(errors.New("synthetic fixture cleanup"))
		startedAt := time.Now()
		f.submit(ctx)
		done := make(chan error, 1)
		go func() {
			_, err := f.current.processEvents(ctx)
			done <- err
		}()
		synctest.Wait()
		select {
		case at := <-f.store.started:
			if elapsed := at.Sub(startedAt); elapsed != 0 {
				t.Errorf("W02 preparation save started after %v; wanted no intentional idle wait", elapsed)
			}
		default:
			t.Error("W02 ready preparation has not reached the owner SaveOperation before idle time advances")
			cancel(errors.New("synthetic expected Red cleanup"))
			<-done
			return
		}
		if len(f.acks) != 0 || len(f.store.savedOperations) != f.baseSave {
			t.Fatal("preparation ACK or completed canonical save crossed the blocked durable boundary")
		}
		close(f.store.release)
		synctest.Wait()
		if len(f.acks) != 1 || <-f.acks != nil || len(f.store.savedOperations) != f.baseSave+1 {
			t.Fatal("durable release did not produce exactly one successful ACK and save")
		}
		// A preparation interrupt must retain both normal collectors' original
		// idle deadlines rather than restarting them after servicing the ACK.
		synctest.Sleep(2 * slurpIdleTimeout)
		synctest.Wait()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if len(f.acks) != 0 {
			t.Fatal("same preparation was acknowledged more than once")
		}
	})
}

func TestPreparationWakeupDuringIdle(t *testing.T) {
	for _, stage := range []string{"inbox", "operation_updates"} {
		t.Run(stage, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newWakeupOwnerFixture(t)
				ctx, cancel := context.WithCancelCause(t.Context())
				defer cancel(errors.New("synthetic fixture cleanup"))
				startedAt := time.Now()
				done := make(chan error, 1)
				go func() {
					_, err := f.current.processEvents(ctx)
					done <- err
				}()
				synctest.Wait()
				if stage == "operation_updates" {
					synctest.Sleep(slurpIdleTimeout)
					synctest.Wait()
				}
				synctest.Sleep(slurpIdleTimeout / 4)
				arrivedAt := time.Now()
				f.submit(ctx)
				synctest.Wait()
				select {
				case at := <-f.store.started:
					if elapsed := at.Sub(arrivedAt); elapsed != 0 {
						t.Errorf("W03 %s preparation save started after %v; remaining idle deadline was used", stage, elapsed)
					}
				default:
					t.Errorf("W03 %s idle collector did not wake for preparation before its original idle deadline", stage)
					cancel(errors.New("synthetic expected Red cleanup"))
					<-done
					return
				}
				if len(f.acks) != 0 || len(f.store.savedOperations) != f.baseSave {
					t.Fatal("ACK or completed save appeared while durable boundary was blocked")
				}
				close(f.store.release)
				synctest.Wait()
				if len(f.acks) != 1 || <-f.acks != nil || len(f.store.savedOperations) != f.baseSave+1 {
					t.Fatal("durable preparation did not produce exactly one ACK and save")
				}
				originalEnd := startedAt.Add(2 * slurpIdleTimeout)
				synctest.Sleep(time.Until(originalEnd) - time.Nanosecond)
				synctest.Wait()
				select {
				case err := <-done:
					t.Fatalf("collector returned before the preserved original idle deadline: %v", err)
				default:
				}
				synctest.Sleep(time.Nanosecond)
				synctest.Wait()
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				default:
					t.Error("preparation restarted or extended a normal collection idle deadline")
					cancel(errors.New("synthetic deadline cleanup"))
					<-done
				}
				if len(f.acks) != 0 {
					t.Fatal("preparation was acknowledged twice")
				}
			})
		})
	}
}

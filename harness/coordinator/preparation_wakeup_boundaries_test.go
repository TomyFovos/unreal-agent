package coordinator

import (
	"context"
	"encoding/json/v2"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
)

// Normal checkpoints from a preceding update/cancel are not preparation saves.
// Keep those visible to the fake Store while retaining the preparation barrier.
type wakeupSelectiveStore struct{ *wakeupDurableStore }

func (s wakeupSelectiveStore) SaveOperation(ctx context.Context, id session.ID, op operation.Operation) error {
	if len(op.ExecutionBasis()) == 0 {
		return s.fakeStore.SaveOperation(ctx, id, op)
	}
	return s.wakeupDurableStore.SaveOperation(ctx, id, op)
}

func wakeupStartProcess(ctx context.Context, f *wakeupOwnerFixture) <-chan error {
	done := make(chan error, 1)
	go func() {
		_, err := f.current.processEvents(ctx)
		done <- err
	}()
	return done
}

func TestPreparationWakeupControlPrefixPrecedesACK(t *testing.T) {
	for _, control := range []string{"hard", "cancel", "when_idle"} {
		t.Run(control, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newWakeupOwnerFixture(t)
				f.current.dependencies.Sessions = wakeupSelectiveStore{f.store}
				close(f.store.release)
				ctx, cancel := context.WithCancelCause(t.Context())
				defer cancel(errors.New("synthetic control fixture cleanup"))
				var input inbox.Input
				switch control {
				case "hard":
					input = stopInput(t, "synthetic-prior-hard-stop", inbox.StopHard)
				case "when_idle":
					input = stopInput(t, "synthetic-prior-idle-stop", inbox.StopWhenIdle)
				case "cancel":
					payload, err := json.Marshal(inbox.ControlMessage{Mode: inbox.CancelOperation, Reason: "synthetic prior cancel", Parameters: inbox.CancelRequest{OperationID: string(f.prepared.ID)}})
					if err != nil {
						t.Fatal(err)
					}
					input = inbox.Input{ID: "synthetic-prior-cancel", Kind: inbox.InputControl, Payload: payload}
				}
				// Both producers are blocked on unbuffered owner channels before
				// processEvents begins. No cross-channel sender-time order is assumed.
				submitTestInput(t, f.current.dependencies.Inbox, input)
				f.submit(ctx)
				synctest.Wait()
				done := wakeupStartProcess(ctx, f)
				synctest.Wait()
				select {
				case ack := <-f.acks:
					if control == "when_idle" {
						if ack != nil || len(f.store.started) != 1 {
							t.Fatal("StopWhenIdle incorrectly rejected a draining preparation", ack)
						}
					} else if !errors.Is(ack, operation.ErrPreparationUncommitted) || len(f.store.started) != 0 {
						t.Fatalf("accepted %s control was overtaken by a successful preparation: ack=%v saves=%d", control, ack, len(f.store.started))
					}
				default:
					t.Error("owner did not service a ready preparation at the finite control-prefix boundary")
				}
				if len(f.store.appendedInputs) != 1 || f.store.appendedInputs[0].ID != input.ID {
					t.Error("ready control prefix was not persisted exactly once before ACK")
				}
				cancel(errors.New("synthetic completed control check"))
				<-done
			})
		})
	}
}

func TestPreparationWakeupReadyTerminalUpdatePrecedesACK(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newWakeupOwnerFixture(t)
		f.current.dependencies.Sessions = wakeupSelectiveStore{f.store}
		close(f.store.release)
		ctx, cancel := context.WithCancelCause(t.Context())
		defer cancel(errors.New("synthetic terminal fixture cleanup"))
		terminal := operation.CloneExecutionMetadata(f.current.state.operations[f.prepared.ID])
		terminal.Status = operation.StatusCompleted
		go func() {
			select {
			case f.manager.updates <- terminal:
			case <-ctx.Done():
			}
		}()
		f.submit(ctx)
		synctest.Wait()
		done := wakeupStartProcess(ctx, f)
		synctest.Wait()
		select {
		case ack := <-f.acks:
			if !errors.Is(ack, operation.ErrPreparationUncommitted) {
				t.Fatal("terminal update did not revoke preparation", ack)
			}
		default:
			t.Error("ready terminal update/preparation did not reach a finite owner boundary")
		}
		if len(f.store.started) != 0 || len(f.store.savedOperations) != f.baseSave+1 || f.current.state.operations[f.prepared.ID].Status != operation.StatusCompleted {
			t.Fatal("terminal prefix was lost, duplicated, or overtaken by a preparation save")
		}
		cancel(errors.New("synthetic completed terminal check"))
		<-done
	})
}

func TestPreparationWakeupNilAndClosedDoNotSpin(t *testing.T) {
	for _, mode := range []string{"no_capability", "nil", "closed"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newWakeupOwnerFixture(t)
				switch mode {
				case "no_capability":
					f.current.dependencies.Operations = f.manager.fakeOperationManager
				case "nil":
					f.manager.prepared = nil
				case "closed":
					close(f.manager.prepared)
				}
				started := time.Now()
				if _, err := f.current.processEvents(t.Context()); err != nil {
					t.Fatal(err)
				}
				if elapsed := time.Since(started); elapsed != 2*slurpIdleTimeout {
					t.Fatalf("%s changed normal idle collection deadline: %v", mode, elapsed)
				}
				if len(f.acks) != 0 || len(f.store.started) != 0 || len(f.store.savedOperations) != f.baseSave {
					t.Fatal("absent/closed preparation channel performed a phantom preparation")
				}
			})
		})
	}
}

func TestPreparationWakeupSaveFailureEndsOwnerWithoutSuccessACK(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newWakeupOwnerFixture(t)
		fault := errors.New("synthetic durable save result unknown")
		f.store.saveOperationErr = fault
		close(f.store.release)
		ctx, cancel := context.WithCancelCause(t.Context())
		defer cancel(errors.New("synthetic save-fault cleanup"))
		f.submit(ctx)
		done := wakeupStartProcess(ctx, f)
		synctest.Wait()
		select {
		case err := <-done:
			if !errors.Is(err, fault) {
				t.Fatal("durable save failure was not propagated", err)
			}
		default:
			t.Error("failed preparation save did not end the owner at the ready boundary")
			cancel(errors.New("synthetic fault test cleanup"))
			<-done
		}
		select {
		case err := <-f.acks:
			if !errors.Is(err, fault) {
				t.Fatal("save failure was converted into a success/generic ACK", err)
			}
		default:
			t.Error("save failure did not deliver its typed error ACK")
		}
		if len(f.current.state.operations[f.prepared.ID].ExecutionBasis()) != 0 || len(f.acks) != 0 {
			t.Fatal("ambiguous save published an in-memory execution basis or duplicated an ACK")
		}
	})
}

func TestPreparationWakeupLongSavePreservesElapsedIdleDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newWakeupOwnerFixture(t)
		ctx, cancel := context.WithCancelCause(t.Context())
		defer cancel(errors.New("synthetic long-save cleanup"))
		startedAt := time.Now()
		done := wakeupStartProcess(ctx, f)
		synctest.Wait()
		synctest.Sleep(slurpIdleTimeout / 4)
		f.submit(ctx)
		synctest.Wait()
		if len(f.store.started) != 1 {
			t.Error("long-save test did not reach preparation while idle")
			cancel(errors.New("synthetic long-save expected Red cleanup"))
			<-done
			return
		}
		// Let the Inbox deadline expire while the owner is genuinely in Save.
		// The Updates phase has not begun, so it still gets its own normal 1ms.
		synctest.Sleep(2 * slurpIdleTimeout)
		if len(f.acks) != 0 {
			t.Fatal("long save acknowledged before durable release")
		}
		close(f.store.release)
		synctest.Wait()
		if len(f.acks) != 1 || <-f.acks != nil {
			t.Fatal("long save did not ACK exactly once")
		}
		synctest.Sleep(slurpIdleTimeout)
		synctest.Wait()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			if elapsed := time.Since(startedAt); elapsed != 13*slurpIdleTimeout/4 {
				t.Fatalf("expired Inbox deadline was restarted: elapsed=%v", elapsed)
			}
		default:
			t.Error("expired idle deadline added a fresh wait after preparation ACK")
			cancel(errors.New("synthetic long-save deadline cleanup"))
			<-done
		}
	})
}

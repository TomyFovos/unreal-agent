package coordinator

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/operation"
)

func TestPreparationWakeupControlAtInboxCapDefinesAcceptedPrefix(t *testing.T) {
	for _, stopAt := range []int{slurpMaxItems, slurpMaxItems + 1} {
		t.Run(fmt.Sprintf("stop_at_%d", stopAt), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newWakeupOwnerFixture(t)
				close(f.store.release)
				ctx, cancel := context.WithCancelCause(t.Context())
				defer cancel(errors.New("synthetic cap control cleanup"))
				for index := 1; index <= slurpMaxItems+1; index++ {
					input := externalEvent(t, index, inbox.ID(fmt.Sprintf("synthetic-cap-%03d", index)), "synthetic input")
					if index == stopAt {
						input = stopInput(t, input.ID, inbox.StopHard)
					}
					submitTestInput(t, f.current.dependencies.Inbox, input)
				}
				f.store.onAppendInput = func(_ inbox.Input) {
					if len(f.store.appendedInputs) == slurpMaxItems {
						f.submit(ctx)
					}
				}
				done := wakeupStartProcess(ctx, f)
				synctest.Wait()
				select {
				case err := <-f.acks:
					if stopAt == slurpMaxItems {
						if !errors.Is(err, operation.ErrPreparationUncommitted) || len(f.store.started) != 0 || f.current.stop.request.Mode != inbox.StopHard {
							t.Fatal("hundredth accepted StopHard was overtaken", err)
						}
					} else if err != nil || len(f.store.started) != 1 || f.current.stop.request.Mode == inbox.StopHard {
						t.Fatal("unreceived 101st input was incorrectly treated as already accepted", err)
					}
				default:
					t.Error("preparation failed to wake the owner after applying its capped Inbox prefix")
				}
				if len(f.store.appendedInputs) != slurpMaxItems {
					t.Fatal("preparation reopened a completed Inbox batch")
				}
				cancel(errors.New("synthetic cap control assertions complete"))
				<-done
			})
		})
	}
}

func TestPreparationWakeupRepeatedCallbackBudgetRetainsOneCheckpoint(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newWakeupOwnerFixture(t)
		close(f.store.release)
		ctx, cancel := context.WithCancelCause(t.Context())
		defer cancel(errors.New("synthetic callback burst cleanup"))
		acks := make(chan error, slurpMaxItems+1)
		go func() {
			for range slurpMaxItems + 1 {
				p := operation.PreparedOperation{Operation: f.prepared, Acknowledge: func(err error) { acks <- err }}
				select {
				case f.manager.prepared <- p:
				case <-ctx.Done():
					return
				}
			}
		}()
		synctest.Wait()
		done := wakeupStartProcess(ctx, f)
		synctest.Wait()
		if len(acks) != slurpMaxItems || len(f.store.savedOperations) != f.baseSave+1 || len(f.store.started) != 1 {
			t.Errorf("same accepted checkpoint should be saved once across a finite callback budget: ACKs=%d saves=%d", len(acks), len(f.store.savedOperations)-f.baseSave)
			cancel(errors.New("synthetic callback burst early cleanup"))
			<-done
			return
		}
		synctest.Sleep(2 * slurpIdleTimeout)
		synctest.Wait()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		default:
			t.Error("callback flood prevented the normal finite owner boundary")
			cancel(errors.New("synthetic callback burst deadline cleanup"))
			<-done
			return
		}
		if len(acks) != slurpMaxItems {
			t.Fatal("101st callback bypassed the finite collection budget")
		}
		next := wakeupStartProcess(ctx, f)
		synctest.Wait()
		if len(acks) != slurpMaxItems+1 || len(f.store.savedOperations) != f.baseSave+1 {
			t.Fatal("deferred callback was lost, saved twice, or forced through an idle wait")
		}
		for range slurpMaxItems + 1 {
			if err := <-acks; err != nil {
				t.Fatal("same accepted checkpoint callback failed", err)
			}
		}
		cancel(errors.New("synthetic callback burst assertions complete"))
		<-next
	})
}

func TestPreparationWakeupCancellationDuringDurableSavePreservesCause(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newWakeupOwnerFixture(t)
		ctx, cancel := context.WithCancelCause(t.Context())
		cause := errors.New("synthetic owner canceled during durable save")
		defer cancel(cause)
		f.submit(ctx)
		done := wakeupStartProcess(ctx, f)
		synctest.Wait()
		if len(f.store.started) != 1 || len(f.acks) != 0 {
			t.Fatal("did not block at the real owner durable save before cancellation")
		}
		cancel(cause)
		synctest.Wait()
		if err := <-done; !errors.Is(err, cause) {
			t.Fatal("owner cancellation cause was lost", err)
		}
		if len(f.acks) != 1 || !errors.Is(<-f.acks, cause) {
			t.Fatal("canceled save did not send exactly one typed error ACK")
		}
		if len(f.current.state.operations[f.prepared.ID].ExecutionBasis()) != 0 || len(f.store.savedOperations) != f.baseSave {
			t.Fatal("canceled undurable save published execution basis")
		}
	})
}

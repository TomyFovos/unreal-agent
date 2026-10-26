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

// These tests exercise owner collection methods and their existing fake durable
// Store. They do not represent a real fsync, native execution or an outer Run
// select. Their purpose is the deterministic prefix/ACK/clock boundary.
func TestPreparationWakeupOwnedRequestPrefixFailureACK(t *testing.T) {
	for _, phase := range []string{"inbox_append", "terminal_update_save"} {
		t.Run(phase, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newWakeupOwnerFixture(t)
				f.current.dependencies.Sessions = wakeupSelectiveStore{f.store}
				fault := errors.New("synthetic preceding prefix persistence outcome unknown")
				ctx, cancel := context.WithCancelCause(t.Context())
				defer cancel(errors.New("synthetic prefix failure cleanup"))
				if phase == "inbox_append" {
					f.store.appendInputErr = fault
					submitTestInput(t, f.current.dependencies.Inbox,
						externalEvent(t, 0, "synthetic-failing-prefix-input", "synthetic input"))
				} else {
					f.store.saveOperationErr = fault
					terminal := operation.CloneExecutionMetadata(f.current.state.operations[f.prepared.ID])
					terminal.Status = operation.StatusCompleted
					go func() {
						select {
						case f.manager.updates <- terminal:
						case <-ctx.Done():
						}
					}()
				}
				synctest.Wait()
				// This is the same method entry as an already-received preparation
				// from Run. The request cannot be left with its producer on failure.
				p := operation.PreparedOperation{Operation: f.prepared, Acknowledge: func(err error) { f.acks <- err }}
				done := make(chan error, 1)
				go func() { _, err := f.current.processEventsWithPreparation(ctx, &p); done <- err }()
				synctest.Wait()
				select {
				case err := <-done:
					if !errors.Is(err, fault) {
						t.Fatalf("preceding %s failure changed identity: %v", phase, err)
					}
				default:
					t.Error("owner did not terminate on the failed preceding prefix")
					cancel(errors.New("synthetic prefix failure unexpected wait"))
					<-done
				}
				select {
				case err := <-f.acks:
					if !errors.Is(err, fault) {
						t.Fatal("owned preparation did not receive the same typed failure ACK", err)
					}
				default:
					t.Error("owned preparation was abandoned without its failure ACK")
				}
				if len(f.acks) != 0 || len(f.store.started) != 0 || len(f.current.state.operations[f.prepared.ID].ExecutionBasis()) != 0 {
					t.Fatal("failed prefix duplicated ACK, started preparation save, or published an unsaved basis")
				}
				if phase == "inbox_append" {
					if len(f.store.appendedInputs) != 1 || len(f.store.savedOperations) != f.baseSave {
						t.Fatal("inbox fault did not occur exactly at the intended prefix append")
					}
				} else if len(f.store.savedOperations) != f.baseSave+1 || f.store.savedOperations[f.baseSave].Status != operation.StatusCompleted || len(f.store.savedOperations[f.baseSave].ExecutionBasis()) != 0 {
					t.Fatal("update fault did not occur at the intended terminal checkpoint before preparation")
				}
			})
		})
	}
}

func TestPreparationWakeupEarlyUpdateDeadlineSurvivesInboxContinuation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newWakeupOwnerFixture(t)
		f.current.dependencies.Sessions = wakeupSelectiveStore{f.store}
		close(f.store.release)
		ctx, cancel := context.WithCancelCause(t.Context())
		defer cancel(errors.New("synthetic early updates cleanup"))
		started := time.Now()
		done := make(chan error, 1)
		go func() { _, err := f.current.processEvents(ctx); done <- err }()
		synctest.Wait()
		synctest.Sleep(slurpIdleTimeout / 4)
		update := operation.WithExecutionBasis(f.prepared, nil)
		update.ID = "synthetic-early-prefix-update"
		update.Status = operation.StatusReady
		go func() {
			select {
			case f.manager.updates <- update:
			case <-ctx.Done():
			}
		}()
		synctest.Wait()
		f.submit(ctx)
		synctest.Wait()
		if len(f.acks) != 1 || <-f.acks != nil || len(f.store.started) != 1 || len(f.store.savedOperations) != f.baseSave+2 {
			t.Fatal("early update prefix and preparation did not each save once before the idle boundary")
		}
		// Updates first received at .25ms have deadline1.25ms. Inbox then
		// receives at .5ms and reaches1.5ms. Updates must not restart at1.5ms.
		synctest.Sleep(slurpIdleTimeout / 4)
		input := externalEvent(t, 0, "synthetic-later-inbox-prefix", "synthetic input")
		submitTestInput(t, f.current.dependencies.Inbox, input)
		synctest.Wait()
		end := started.Add(3 * slurpIdleTimeout / 2)
		synctest.Sleep(time.Until(end) - time.Nanosecond)
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("Inbox continuation ended before its normal reset deadline: %v", err)
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
			t.Error("early Updates prefix deadline was restarted at the later normal phase")
			cancel(errors.New("synthetic early Updates deadline cleanup"))
			<-done
			return
		}
		if elapsed := time.Since(started); elapsed != 3*slurpIdleTimeout/2 {
			t.Fatal("early prefix changed the intended absolute collection deadline", elapsed)
		}
		count := 0
		for _, saved := range f.store.savedOperations {
			if saved.ID == update.ID {
				count++
			}
		}
		if count != 1 || len(f.store.appendedInputs) != 1 || f.store.appendedInputs[0].ID != input.ID || len(f.store.savedOperations) != f.baseSave+2 || len(f.acks) != 0 {
			t.Fatal("already-applied input/update/preparation prefix was lost or saved again")
		}
	})
}

func TestPreparationWakeupClosedOrdinaryPrefixesRemainAppliedOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newWakeupOwnerFixture(t)
		f.current.dependencies.Sessions = wakeupSelectiveStore{f.store}
		close(f.store.release)
		inputs := make(chan inbox.Input, 2)
		updates := make(chan operation.Operation, 2)
		for _, id := range []inbox.ID{"synthetic-closed-prefix-first", "synthetic-closed-prefix-second"} {
			inputs <- externalEvent(t, 0, id, "synthetic input")
		}
		for _, id := range []operation.ID{"synthetic-closed-update-first", "synthetic-closed-update-second"} {
			update := operation.WithExecutionBasis(f.prepared, nil)
			update.ID, update.Status = id, operation.StatusCompleted
			updates <- update
		}
		close(inputs)
		close(updates)
		c := ownerEventCollection{
			inbox:   eventCollection[inbox.Input]{output: inputs},
			updates: eventCollection[operation.Operation]{output: updates},
		}
		defer c.inbox.stop()
		defer c.updates.stop()
		at := time.Now()
		p := operation.PreparedOperation{Operation: f.prepared, Acknowledge: func(err error) { f.acks <- err }}
		if err := c.servicePreparation(t.Context(), f.current, p); err != nil {
			t.Fatal(err)
		}
		if err := collectOwnerPhase(t.Context(), f.current, &c, &c.inbox); err != nil {
			t.Fatal(err)
		}
		if err := c.applyInbox(t.Context(), f.current); err != nil {
			t.Fatal(err)
		}
		if err := collectOwnerPhase(t.Context(), f.current, &c, &c.updates); err != nil {
			t.Fatal(err)
		}
		if err := c.applyUpdates(t.Context(), f.current); err != nil {
			t.Fatal(err)
		}
		if time.Since(at) != 0 || !c.inbox.closed || !c.updates.closed || c.inbox.received != 2 || c.inbox.applied != 2 || c.updates.received != 2 || c.updates.applied != 2 || len(c.inbox.pending) != 0 || len(c.updates.pending) != 0 {
			t.Fatal("closed ordinary stream lost a prefix, restarted idle, or replayed its processed position")
		}
		if len(f.store.appendedInputs) != 2 || f.store.appendedInputs[0].ID != "synthetic-closed-prefix-first" || f.store.appendedInputs[1].ID != "synthetic-closed-prefix-second" {
			t.Fatal("closed Inbox prefix was lost, duplicated or reordered")
		}
		if len(f.store.savedOperations) != f.baseSave+3 || f.store.savedOperations[f.baseSave].ID != "synthetic-closed-update-first" || f.store.savedOperations[f.baseSave+1].ID != "synthetic-closed-update-second" || len(f.acks) != 1 || <-f.acks != nil {
			t.Fatal("closed Updates prefix or durable preparation was lost, duplicated or reordered")
		}
	})
}

func TestOwnerCollectionExpiredDeadlineRetainsSimultaneouslyReadyTail(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newWakeupOwnerFixture(t)
		f.manager.prepared = nil
		values := make(chan int, 1)
		values <- 7
		at := time.Now()
		phase := eventCollection[int]{output: values, started: true, deadline: at, timer: time.NewTimer(0)}
		defer phase.stop()
		// Both a value and deadline are ready. The owner rule stops an expired
		// phase before selecting more data. This is deliberately not a claim
		// that legacy select's random simultaneous-ready choice was identical.
		if err := collectOwnerPhase(t.Context(), f.current, &ownerEventCollection{}, &phase); err != nil {
			t.Fatal(err)
		}
		if !phase.done || phase.received != 0 || phase.applied != 0 || len(phase.pending) != 0 || time.Since(at) != 0 {
			t.Fatal("expired deadline consumed the next prefix or introduced a new idle wait")
		}
		select {
		case got := <-values:
			if got != 7 {
				t.Fatal("ready tail changed", got)
			}
		default:
			t.Fatal("unreceived simultaneous-ready tail disappeared")
		}
	})
}

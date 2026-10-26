package coordinator

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/operation"
)

func TestPreparationWakeupInboxCapAndTailSurviveInterrupt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newWakeupOwnerFixture(t)
		close(f.store.release)
		ctx, cancel := context.WithCancelCause(t.Context())
		defer cancel(errors.New("synthetic batch fixture cleanup"))
		for index := range slurpMaxItems + 1 {
			input := externalEvent(t, index, inbox.ID(fmt.Sprintf("synthetic-prefix-%03d", index)), "synthetic input")
			submitTestInput(t, f.current.dependencies.Inbox, input)
		}
		// The preparation only becomes ready after the original Inbox batch
		// cap was reached and its hundredth item is being persisted. Its wake-up
		// boundary must not reset the cap and consume the 101st tail item.
		f.store.onAppendInput = func(_ inbox.Input) {
			if len(f.store.appendedInputs) == slurpMaxItems {
				f.submit(ctx)
			}
		}
		startedAt := time.Now()
		result := make(chan struct {
			call bool
			err  error
		}, 1)
		go func() {
			call, err := f.current.processEvents(ctx)
			result <- struct {
				call bool
				err  error
			}{call, err}
		}()
		synctest.Wait()
		select {
		case err := <-f.acks:
			if err != nil {
				t.Fatal(err)
			}
		default:
			t.Error("preparation did not wake the Updates collection after a full Inbox prefix")
		}
		if got := len(f.store.appendedInputs); got != slurpMaxItems {
			t.Fatalf("Inbox prefix cap changed across preparation: got=%d want=%d", got, slurpMaxItems)
		}
		for index, input := range f.store.appendedInputs {
			if input.ID != inbox.ID(fmt.Sprintf("synthetic-prefix-%03d", index)) {
				t.Fatal("Inbox prefix FIFO changed")
			}
		}
		select {
		case <-result:
			t.Fatal("preparation ACK incorrectly ended the ordinary model decision boundary")
		default:
		}
		synctest.Sleep(slurpIdleTimeout)
		synctest.Wait()
		select {
		case got := <-result:
			if got.err != nil || !got.call {
				t.Fatal("normal input/model decision was lost", got.err, got.call)
			}
		default:
			t.Error("ordinary Updates idle deadline was extended by preparation")
			cancel(errors.New("synthetic batch deadline cleanup"))
			<-result
			return
		}
		if elapsed := time.Since(startedAt); elapsed != slurpIdleTimeout {
			t.Fatal("full Inbox batch unnecessarily waited its own idle timer", elapsed)
		}
		// The next normal boundary owns exactly the remaining tail; no replay of
		// the already-persisted prefix and no extra preparation save/ACK.
		if _, err := f.current.processEvents(ctx); err != nil {
			t.Fatal(err)
		}
		if len(f.store.appendedInputs) != slurpMaxItems+1 || f.store.appendedInputs[slurpMaxItems].ID != "synthetic-prefix-100" {
			t.Fatal("unprocessed Inbox tail was lost or prefix was applied twice")
		}
		if len(f.store.savedOperations) != f.baseSave+1 || len(f.acks) != 0 {
			t.Fatal("preparation was duplicated while resuming normal collection")
		}
	})
}

func TestPreparationWakeupReturnsToUpdatesBetweenServiceUnits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newWakeupOwnerFixture(t)
		close(f.store.release)
		f.current.dependencies.Sessions = wakeupSelectiveStore{f.store}
		// This buffered single-slot update is deliberately a deterministic
		// boundary oracle, not an assertion that real unbuffered channels expose
		// their queue through len(). Unbuffered priority is tested separately.
		f.manager.updates = make(chan operation.Operation, 1)
		second := operation.CloneExecutionMetadata(f.prepared)
		second.ID = "synthetic-second-wakeup"
		prior := operation.WithExecutionBasis(second, nil)
		f.current.addOperationToLocalState(prior)
		ctx, cancel := context.WithCancelCause(t.Context())
		defer cancel(errors.New("synthetic fairness fixture cleanup"))
		acks := make(chan struct {
			id  operation.ID
			err error
		}, 2)
		first := operation.PreparedOperation{Operation: f.prepared, Acknowledge: func(err error) {
			acks <- struct {
				id  operation.ID
				err error
			}{f.prepared.ID, err}
			terminal := operation.CloneExecutionMetadata(prior)
			terminal.Status = operation.StatusCompleted
			f.manager.updates <- terminal
		}}
		next := operation.PreparedOperation{Operation: second, Acknowledge: func(err error) {
			acks <- struct {
				id  operation.ID
				err error
			}{second.ID, err}
		}}
		go func() {
			for _, p := range []operation.PreparedOperation{first, next} {
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
		if len(acks) != 2 {
			t.Error("each preparation service unit should return to normal updates without requiring an idle timeout")
		} else {
			a, b := <-acks, <-acks
			if a.id != f.prepared.ID || a.err != nil || b.id != second.ID || !errors.Is(b.err, operation.ErrPreparationUncommitted) {
				t.Fatal("ready normal update did not precede the next preparation ACK", a, b)
			}
		}
		if f.current.state.operations[second.ID].Status != operation.StatusCompleted || len(f.store.started) != 1 {
			t.Error("terminal update starved or second preparation was saved before the update")
		}
		cancel(errors.New("synthetic fairness assertions complete"))
		<-done
	})
}

func TestPreparationWakeupFiniteBurstPreservesNormalBoundaryAndTail(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newWakeupOwnerFixture(t)
		close(f.store.release)
		f.store.started = make(chan time.Time, slurpMaxItems+1)
		ctx, cancel := context.WithCancelCause(t.Context())
		defer cancel(errors.New("synthetic preparation burst cleanup"))
		acks := make(chan operation.ID, slurpMaxItems+1)
		values := make([]operation.PreparedOperation, 0, slurpMaxItems+1)
		for index := range slurpMaxItems + 1 {
			prepared := operation.CloneExecutionMetadata(f.prepared)
			prepared.ID = operation.ID(fmt.Sprintf("synthetic-burst-%03d", index))
			f.current.addOperationToLocalState(operation.WithExecutionBasis(prepared, nil))
			values = append(values, operation.PreparedOperation{Operation: prepared, Acknowledge: func(err error) {
				if err != nil {
					t.Errorf("eligible preparation was rejected: %v", err)
				}
				acks <- prepared.ID
			}})
		}
		go func() {
			for _, p := range values {
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
		if len(acks) != slurpMaxItems {
			t.Errorf("finite preparation service budget: ACKs=%d want=%d before normal idle boundary", len(acks), slurpMaxItems)
			cancel(errors.New("synthetic preparation-burst expected Red cleanup"))
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
			t.Error("continuous preparation availability prevented the finite normal model decision boundary")
			cancel(errors.New("synthetic preparation burst deadline cleanup"))
			<-done
			return
		}
		if len(acks) != slurpMaxItems {
			t.Fatal("preparation tail escaped the per-boundary budget")
		}
		second := wakeupStartProcess(ctx, f)
		synctest.Wait()
		if len(acks) != slurpMaxItems+1 || len(f.store.savedOperations) != f.baseSave+slurpMaxItems+1 {
			t.Fatal("deferred preparation tail was lost, duplicated, or forced to wait an additional idle deadline")
		}
		for index := range slurpMaxItems + 1 {
			if got := <-acks; got != operation.ID(fmt.Sprintf("synthetic-burst-%03d", index)) {
				t.Fatal("single producer preparation FIFO changed", got)
			}
		}
		cancel(errors.New("synthetic preparation burst assertions complete"))
		<-second
	})
}

package coordinator

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

func TestPreparationWakeupOrdinaryInputStillResetsOriginalIdle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newWakeupOwnerFixture(t)
		close(f.store.release)
		ctx, cancel := context.WithCancelCause(t.Context())
		defer cancel(errors.New("synthetic ordinary reset cleanup"))
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
		synctest.Sleep(slurpIdleTimeout / 4)
		f.submit(ctx)
		synctest.Wait()
		if len(f.acks) != 1 || <-f.acks != nil || f.current.state.callModel {
			t.Fatal("preparation either failed or incorrectly requested an early model generation")
		}
		synctest.Sleep(slurpIdleTimeout / 4)
		input := externalEvent(t, 0, "synthetic-reset-input", "synthetic normal input")
		submitTestInput(t, f.current.dependencies.Inbox, input)
		synctest.Wait()
		// Inbox reset at .5ms => Inbox completes1.5ms, Updates completes2.5ms.
		end := startedAt.Add(5 * slurpIdleTimeout / 2)
		synctest.Sleep(time.Until(end) - time.Nanosecond)
		synctest.Wait()
		select {
		case got := <-result:
			t.Fatalf("ordinary input lost its full idle reset interval: call=%v error=%v", got.call, got.err)
		default:
		}
		synctest.Sleep(time.Nanosecond)
		synctest.Wait()
		select {
		case got := <-result:
			if !got.call || got.err != nil {
				t.Fatal("ordinary input did not reach its one normal model decision boundary", got.call, got.err)
			}
		default:
			t.Error("preparation extended the normal input reset deadline")
			cancel(errors.New("synthetic normal reset deadline cleanup"))
			<-result
		}
		if len(f.store.appendedInputs) != 1 || f.store.appendedInputs[0].ID != input.ID || len(f.store.savedOperations) != f.baseSave+1 || len(f.acks) != 0 {
			t.Fatal("normal prefix or preparation was omitted/duplicated around the reset")
		}
	})
}

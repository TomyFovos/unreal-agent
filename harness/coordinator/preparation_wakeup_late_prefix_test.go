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

func TestPreparationWakeupLateInboxPrefixDoesNotReopenExpiredPhase(t *testing.T) {
	for _, mode := range []string{"normal", "hard_stop"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newWakeupOwnerFixture(t)
				close(f.store.release)
				ctx, cancel := context.WithCancelCause(t.Context())
				defer cancel(errors.New("synthetic late prefix cleanup"))
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
				synctest.Sleep(5 * slurpIdleTimeout / 4)
				synctest.Wait()
				input := externalEvent(t, 0, "synthetic-late-input", "synthetic normal input")
				if mode == "hard_stop" {
					input = stopInput(t, input.ID, inbox.StopHard)
				}
				submitTestInput(t, f.current.dependencies.Inbox, input)
				synctest.Wait() // Its unbuffered output is ready after Inbox completed.
				f.submit(ctx)
				synctest.Wait()
				select {
				case err := <-f.acks:
					if mode == "hard_stop" {
						if !errors.Is(err, operation.ErrPreparationUncommitted) || len(f.store.started) != 0 {
							t.Fatal("late ready hard stop was overtaken", err)
						}
					} else if err != nil || len(f.store.started) != 1 {
						t.Fatal("late normal input incorrectly rejected preparation", err)
					}
				default:
					t.Error("Updates idle did not wake for preparation with a late Inbox prefix")
				}
				if len(f.store.appendedInputs) != 1 || f.store.appendedInputs[0].ID != input.ID {
					t.Fatal("ready late Inbox prefix was not persisted exactly once before ACK")
				}
				end := startedAt.Add(2 * slurpIdleTimeout)
				synctest.Sleep(time.Until(end) - time.Nanosecond)
				synctest.Wait()
				select {
				case <-result:
					t.Fatal("late-prefix preparation ended the ordinary Updates boundary early")
				default:
				}
				synctest.Sleep(time.Nanosecond)
				synctest.Wait()
				select {
				case got := <-result:
					if got.err != nil {
						t.Fatal(got.err)
					}
					if mode == "normal" && !got.call {
						t.Fatal("late normal input was lost at the ordinary model decision boundary")
					}
				default:
					t.Error("late ready Inbox prefix reopened an already-completed normal phase")
					cancel(errors.New("synthetic late prefix deadline cleanup"))
					<-result
				}
			})
		})
	}
}

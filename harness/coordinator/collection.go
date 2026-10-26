package coordinator

import (
	"context"
	"fmt"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/operation"
)

// A collection belongs to the owner stack, for one processEvents decision. Its
// received count survives prefix application; preparations never reset its idle
// deadline. Previously applied values are cleared rather than retained/replayed.
type eventCollection[T any] struct {
	output   <-chan T
	pending  []T
	received int
	applied  int
	deadline time.Time
	timer    *time.Timer
	started  bool
	done     bool
	closed   bool
}

func (c *eventCollection[T]) start() {
	if !c.started {
		c.started = true
		c.deadline = time.Now().Add(slurpIdleTimeout)
	}
}

func (c *eventCollection[T]) accept(value T, open bool) {
	if !open {
		c.closed, c.done = true, true
		return
	}
	c.pending = append(c.pending, value)
	c.received++
	// A priority prefix can receive from the already-finished Inbox while the
	// Updates phase is active. That does not reopen a completed normal phase.
	if !c.done {
		c.started = true
		c.deadline = time.Now().Add(slurpIdleTimeout)
	}
	if c.received == slurpMaxItems {
		c.done = true
	}
}

func (c *eventCollection[T]) stop() {
	if c.timer != nil {
		c.timer.Stop()
	}
}

func (c *eventCollection[T]) appliedPrefix() {
	c.applied += len(c.pending)
	clear(c.pending)
	c.pending = c.pending[:0]
}

// receiveReady defines a finite priority boundary, not a global order across
// independent channels. In particular len(unbufferedChannel) says nothing about
// ready senders. The 101st value remains owned by its sender for a later round.
func (c *eventCollection[T]) receiveReady(ctx context.Context) error {
	for !c.closed && c.received < slurpMaxItems {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case value, open := <-c.output:
			c.accept(value, open)
		default:
			return nil
		}
	}
	return nil
}

func (current *coordinator) preparationRequests() <-chan operation.PreparedOperation {
	if !current.preparationsClosed {
		if manager, ok := current.dependencies.Operations.(operation.PreparationManager); ok {
			return manager.Preparations()
		}
	}
	return nil
}

type ownerEventCollection struct {
	inbox    eventCollection[inbox.Input]
	updates  eventCollection[operation.Operation]
	services int
}

func (current *coordinator) collectEvents(ctx context.Context, prepared *operation.PreparedOperation) error {
	c := ownerEventCollection{
		inbox:   eventCollection[inbox.Input]{output: current.dependencies.Inbox.Output()},
		updates: eventCollection[operation.Operation]{output: current.dependencies.Operations.Updates()},
	}
	defer c.inbox.stop()
	defer c.updates.stop()
	c.inbox.start()
	if prepared != nil {
		if err := c.servicePreparation(ctx, current, *prepared); err != nil {
			return err
		}
	}
	if err := collectOwnerPhase(ctx, current, &c, &c.inbox); err != nil {
		return fmt.Errorf("slurp inbox: %w", err)
	}
	if err := c.applyInbox(ctx, current); err != nil {
		return err
	}
	if err := collectOwnerPhase(ctx, current, &c, &c.updates); err != nil {
		return fmt.Errorf("slurp operation updates: %w", err)
	}
	return c.applyUpdates(ctx, current)
}

func (c *ownerEventCollection) applyInbox(ctx context.Context, current *coordinator) error {
	if len(c.inbox.pending) != 0 {
		if err := current.processInputs(ctx, c.inbox.pending); err != nil {
			return err
		}
		c.inbox.appliedPrefix()
	}
	return nil
}

func (c *ownerEventCollection) applyUpdates(ctx context.Context, current *coordinator) error {
	if len(c.updates.pending) != 0 {
		if err := current.processOperations(ctx, c.updates.pending); err != nil {
			return err
		}
		c.updates.appliedPrefix()
	}
	return nil
}

func (c *ownerEventCollection) servicePreparation(ctx context.Context, current *coordinator, prepared operation.PreparedOperation) error {
	// An already-received preparation must get an error ACK if processing its
	// preceding prefix fails. No model/reconcile/bridge decision occurs here.
	err := c.inbox.receiveReady(ctx)
	if err == nil {
		err = c.applyInbox(ctx, current)
	}
	if err == nil {
		err = c.updates.receiveReady(ctx)
	}
	if err == nil {
		err = c.applyUpdates(ctx, current)
	}
	if err == nil {
		err = context.Cause(ctx)
	}
	if err != nil {
		if prepared.Acknowledge != nil {
			prepared.Acknowledge(err)
		}
		return err
	}
	c.services++
	return current.commitOperationPreparation(ctx, prepared)
}

func collectOwnerPhase[T any](ctx context.Context, current *coordinator, c *ownerEventCollection, phase *eventCollection[T]) error {
	phase.start()
	for !phase.done {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		remaining := time.Until(phase.deadline)
		if remaining <= 0 {
			phase.done = true
			break
		}
		if phase.timer == nil {
			phase.timer = time.NewTimer(remaining)
		} else {
			phase.timer.Reset(remaining)
		}
		preparations := current.preparationRequests()
		// Each service handles one preparation and first offers ctx and both
		// ordinary channels their remaining finite prefix. Bound the round too,
		// so an infinite ready preparation producer cannot prevent the normal
		// reconcile/barrier decision (including under a virtual clock).
		if c.services == slurpMaxItems {
			preparations = nil
		}
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case value, open := <-phase.output:
			phase.accept(value, open)
		case prepared, open := <-preparations:
			if !open {
				current.preparationsClosed = true
				continue
			}
			if err := c.servicePreparation(ctx, current, prepared); err != nil {
				return err
			}
		case <-phase.timer.C:
			phase.done = true
		}
	}
	return nil
}

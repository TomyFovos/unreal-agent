package operation

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"errors"
	"sync"
)

// PreparationRequest is executor metadata, not an execution authorization.
// Only the selected local handler can submit one. Result acknowledges a durable
// owner checkpoint; sending the request or receiving it is not that checkpoint.
type PreparationRequest struct {
	ID     ID
	Basis  jsontext.Value
	Result chan PreparationResult
}

type PreparationResult struct {
	Operation Operation
	Err       error
}

type PreparingRemoteJobHandler interface {
	RemoteJobPreparations() <-chan PreparationRequest
}

type PreparedOperation struct {
	Operation   Operation
	Acknowledge func(error)
	pending     *pendingPreparation
}

type PreparationManager interface {
	Preparations() <-chan PreparedOperation
}

// RecoveryPlanner performs only canonical/typed-plan inspection. Filesystem
// proof belongs to the revocable execution lease, outside the owner actor.
type RecoveryPlanner interface {
	PlanRecovery(context.Context, Operation, []UncertainEffect) (Operation, error)
}

func (manager *LocalOperationManager) PlanRecovery(ctx context.Context, op Operation, effects []UncertainEffect) (Operation, error) {
	if p, ok := manager.recoveryHandler(op).(RecoveryPlanner); ok {
		return p.PlanRecovery(ctx, op, effects)
	}
	return op, ErrIndependenceUnproven
}

func (manager *LocalOperationManager) Preparations() <-chan PreparedOperation {
	return manager.prepared
}

type remotePreparation struct {
	handler int
	request PreparationRequest
}

type preparationAck struct {
	pending *pendingPreparation
	err     error
}

type pendingPreparation struct {
	request  PreparationRequest
	prepared Operation
	stop     func() bool
}

var ErrPreparationUncommitted = errors.New("execution preparation is not committed")

func (manager *LocalOperationManager) queuePreparation(event remotePreparation, operations map[ID]*localRunningOperation, pending map[ID]*pendingPreparation) *PreparedOperation {
	r := event.request
	current := operations[r.ID]
	reply := func(err error) {
		var op Operation
		if current != nil {
			op = CloneExecutionMetadata(current.operation)
		}
		deliverPreparationResult(r.Result, PreparationResult{Operation: op, Err: err})
	}
	if current == nil || current.remoteJobHandlerIndex != event.handler || r.Result == nil || cap(r.Result) < 1 || pending[r.ID] != nil || len(pending) >= 128 || current.executionPrepared || !current.operation.RequiresPreparation() || current.ctx.Err() != nil || len(r.Basis) == 0 || len(r.Basis) > 256<<10 || !r.Basis.IsValid() {
		reply(ErrPreparationUncommitted)
		return nil
	}
	if old := current.operation.ExecutionBasis(); len(old) != 0 && !bytes.Equal(old, r.Basis) {
		reply(ErrPreparationUncommitted)
		return nil
	}
	p := &pendingPreparation{request: r, prepared: WithExecutionBasis(current.operation, r.Basis)}
	pending[r.ID] = p
	var once sync.Once
	ack := func(err error) {
		once.Do(func() {
			select {
			case manager.preparationAcks <- preparationAck{pending: p, err: err}:
			case <-manager.ctx.Done():
			}
		})
	}
	p.stop = context.AfterFunc(current.ctx, func() { ack(ErrPreparationUncommitted) })
	return &PreparedOperation{Operation: CloneExecutionMetadata(p.prepared), Acknowledge: ack, pending: p}
}

// Canceled preparations must not retain bases and closures while the owner is
// detached or busy. The pending gate bounds live entries; pruning also bounds
// queued entries across repeated prepare/cancel cycles.
func prunePreparations(queue []PreparedOperation, pending map[ID]*pendingPreparation) []PreparedOperation {
	kept := queue[:0]
	for _, value := range queue {
		if value.pending != nil && pending[value.Operation.ID] == value.pending {
			kept = append(kept, value)
		}
	}
	clear(queue[len(kept):])
	return kept
}

func (manager *LocalOperationManager) finishPreparation(ack preparationAck, operations map[ID]*localRunningOperation, pending map[ID]*pendingPreparation) {
	p := ack.pending
	if pending[p.request.ID] != p {
		return // Stale/double response cannot authorize another operation.
	}
	delete(pending, p.request.ID)
	p.stop()
	err := ack.err
	current := operations[p.request.ID]
	var op Operation
	if current == nil {
		err = ErrPreparationUncommitted
	} else {
		if current.ctx.Err() != nil {
			err = ErrPreparationUncommitted
		}
		if err == nil {
			current.operation = CloneExecutionMetadata(p.prepared)
			current.executionPrepared = true
		}
		op = CloneExecutionMetadata(current.operation)
	}
	if !deliverPreparationResult(p.request.Result, PreparationResult{Operation: op, Err: err}) && current != nil {
		current.cancel()
	}
}

// A malformed custom handler cannot block the manager actor with a reused
// reply channel. Native uses a fresh buffered channel owned by its worker.
func deliverPreparationResult(channel chan PreparationResult, result PreparationResult) (delivered bool) {
	defer func() { _ = recover() }() // A custom handler may have closed its channel.
	select {
	case channel <- result:
		return true
	default:
		return false
	}
}

func (router *remoteJobHandlers) forwardPreparations(ctx context.Context, index int, handler PreparingRemoteJobHandler) {
	for {
		select {
		case request, open := <-handler.RemoteJobPreparations():
			if !open {
				return
			}
			select {
			case router.preparations <- remotePreparation{handler: index, request: request}:
			case <-ctx.Done():
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

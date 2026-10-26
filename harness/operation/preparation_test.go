package operation

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"testing"
)

func TestPreparationRequiresExplicitOwnerAcknowledgement(t *testing.T) {
	for _, mode := range []string{"committed", "append-failed", "lease-revoked"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			manager := &LocalOperationManager{ctx: t.Context(), preparationAcks: make(chan preparationAck, 2)}
			op := WithPreparationRequired(Operation{ID: "synthetic", Type: TypeRemoteJob}, true)
			current := &localRunningOperation{ctx: ctx, cancel: cancel, operation: op, remoteJobHandlerIndex: 1}
			operations := map[ID]*localRunningOperation{op.ID: current}
			pending := map[ID]*pendingPreparation{}
			result := make(chan PreparationResult, 1)
			basis := jsontext.Value(`{"version":1}`)
			p := manager.queuePreparation(remotePreparation{handler: 1, request: PreparationRequest{ID: op.ID, Basis: basis, Result: result}}, operations, pending)
			if p == nil || len(current.operation.ExecutionBasis()) != 0 || current.executionPrepared {
				t.Fatal("receiving preparation changed execution authority")
			}
			select {
			case <-result:
				t.Fatal("channel delivery was treated as durable acknowledgement")
			default:
			}
			var failure error
			if mode == "append-failed" {
				failure = errors.New("synthetic append unknown")
			}
			if mode == "lease-revoked" {
				cancel()
			}
			p.Acknowledge(failure)
			ack := <-manager.preparationAcks
			manager.finishPreparation(ack, operations, pending)
			reply := <-result
			if mode == "committed" {
				if reply.Err != nil || !current.executionPrepared || string(reply.Operation.ExecutionBasis()) != string(basis) {
					t.Fatal("successful canonical acknowledgement was not installed")
				}
				p.Operation.ExecutionBasis()[2] = 'X'
				if string(current.operation.ExecutionBasis()) != string(basis) {
					t.Fatal("owner publication aliases manager evidence")
				}
			} else if reply.Err == nil || current.executionPrepared || len(current.operation.ExecutionBasis()) != 0 {
				t.Fatal("failed or revoked acknowledgement authorized execution")
			}
			p.Acknowledge(nil)
			manager.finishPreparation(ack, operations, pending)
			select {
			case <-result:
				t.Fatal("stale acknowledgement was delivered twice")
			default:
			}
		})
	}
}

func TestPreparationQueueReleasesCanceledEntries(t *testing.T) {
	queue := make([]PreparedOperation, 0, 128)
	pending := map[ID]*pendingPreparation{}
	for range 1000 {
		p := &pendingPreparation{}
		pending["synthetic"] = p
		queue = append(queue, PreparedOperation{Operation: Operation{ID: "synthetic"}, pending: p})
		delete(pending, "synthetic")
		queue = prunePreparations(queue, pending)
		if len(queue) != 0 || cap(queue) > 128 {
			t.Fatal("canceled preparation retained unbounded basis closures")
		}
	}
}

func TestPreparationMalformedReplyDoesNotBlockActor(t *testing.T) {
	for _, mode := range []string{"nil", "unbuffered", "prefilled", "closed"} {
		t.Run(mode, func(t *testing.T) {
			var channel chan PreparationResult
			switch mode {
			case "unbuffered":
				channel = make(chan PreparationResult)
			case "prefilled":
				channel = make(chan PreparationResult, 1)
				channel <- PreparationResult{}
			case "closed":
				channel = make(chan PreparationResult, 1)
				close(channel)
			}
			if deliverPreparationResult(channel, PreparationResult{}) {
				t.Fatal("malformed reply was treated as delivered")
			}
		})
	}
}

func TestPreparationRejectsUnselectedOrMalformedWorker(t *testing.T) {
	for _, mode := range []string{"wrong-handler", "missing-flag", "empty-basis", "invalid-json", "changed-basis", "duplicate", "revoked", "unknown-id"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			op := WithPreparationRequired(Operation{ID: "synthetic"}, true)
			current := &localRunningOperation{ctx: ctx, operation: op, remoteJobHandlerIndex: 1}
			operations := map[ID]*localRunningOperation{op.ID: current}
			pending := map[ID]*pendingPreparation{}
			result := make(chan PreparationResult, 1)
			event := remotePreparation{handler: 1, request: PreparationRequest{ID: op.ID, Basis: jsontext.Value(`{}`), Result: result}}
			switch mode {
			case "wrong-handler":
				event.handler = 2
			case "missing-flag":
				current.operation = WithPreparationRequired(op, false)
			case "empty-basis":
				event.request.Basis = nil
			case "invalid-json":
				event.request.Basis = jsontext.Value(`{`)
			case "changed-basis":
				current.operation = WithExecutionBasis(op, jsontext.Value(`{"original":true}`))
			case "duplicate":
				pending[op.ID] = &pendingPreparation{}
			case "revoked":
				cancel()
			case "unknown-id":
				event.request.ID = "other"
			}
			manager := &LocalOperationManager{ctx: t.Context()}
			if manager.queuePreparation(event, operations, pending) != nil {
				t.Fatal("invalid preparation accepted")
			}
			if reply := <-result; !errors.Is(reply.Err, ErrPreparationUncommitted) || current.executionPrepared {
				t.Fatal("invalid preparation did not fail closed")
			}
		})
	}
}

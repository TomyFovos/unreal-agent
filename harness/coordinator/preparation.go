package coordinator

import (
	"bytes"
	"context"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/operation"
)

// This is the sole durable acknowledgement. Neither a channel send nor manager
// acceptance can start a prepared native mutation. Save failure (including an
// unknown append outcome) returns an error ACK and ends this owner lifetime.
func (current *coordinator) commitOperationPreparation(ctx context.Context, p operation.PreparedOperation) error {
	if p.Acknowledge == nil {
		return operation.ErrPreparationUncommitted
	}
	op, ok := current.state.operations[p.Operation.ID]
	if !ok || !op.RequiresPreparation() || operationIsTerminal(op.Status) || op.Status == operation.StatusCanceling || current.stop.request.Mode == inbox.StopHard || current.stop.cancellationRequested || ctx.Err() != nil {
		p.Acknowledge(operation.ErrPreparationUncommitted)
		return nil
	}
	// The manager constructs every other field from its immutable accepted op.
	// Do not accept a stale request after a different owner decision/checkpoint.
	if op.Type != p.Operation.Type || op.Version != p.Operation.Version || op.ToolName != p.Operation.ToolName || !bytes.Equal(op.State, p.Operation.State) || !bytes.Equal(op.Idempotency(), p.Operation.Idempotency()) || len(p.Operation.ExecutionBasis()) == 0 {
		p.Acknowledge(operation.ErrPreparationUncommitted)
		return operation.ErrPreparationUncommitted
	}
	if !preparedExecutionMetadataMatches(op, p.Operation) || len(op.ExecutionBasis()) != 0 && !bytes.Equal(op.ExecutionBasis(), p.Operation.ExecutionBasis()) {
		p.Acknowledge(operation.ErrPreparationUncommitted)
		return operation.ErrPreparationUncommitted
	}
	if !operation.SameExecutionMetadata(op, p.Operation) {
		if err := current.storeOperationInSessionStore(ctx, p.Operation); err != nil {
			p.Acknowledge(err)
			return err
		}
		current.addOperationToLocalState(p.Operation)
	}
	p.Acknowledge(nil)
	return nil
}

// preparedExecutionMetadataMatches borrows slices only for this synchronous,
// read-only comparison. No borrowed metadata is stored, acknowledged or returned.
// Published preparations still use the manager's independently owned value.
func preparedExecutionMetadataMatches(current, prepared operation.Operation) bool {
	expected := current
	metadata := operation.ExecutionMetadata{}
	if current.Execution != nil {
		metadata = *current.Execution
	}
	metadata.ExecutionBasis = prepared.ExecutionBasis()
	expected.Execution = &metadata
	return operation.SameExecutionMetadata(expected, prepared)
}

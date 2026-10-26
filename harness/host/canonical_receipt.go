package host

import (
	"bytes"
	"context"

	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

const maxStartupReceiptInspections = 256

// reconcileCanonicalReceipts runs only for an explicitly selected backend and
// before its Coordinator starts. Host.Open already owns the Session writer and
// has loaded accepted operations; no default-path history scan/read is added.
func (s *Session) reconcileCanonicalReceipts(ctx context.Context, restored sessionstore.ResumeState, accepted []operation.Operation) (sessionstore.ResumeState, error) {
	inspector, enabled := s.runtime.Operations.(operation.CanonicalReceiptInspector)
	if !enabled || len(accepted) == 0 {
		return restored, nil
	}
	positions := make(map[operation.ID]int, len(restored.Operations))
	for i, op := range restored.Operations {
		positions[op.ID] = i
	}
	store := &serializedStore{s}
	for i, prior := range accepted {
		if i >= maxStartupReceiptInspections {
			// Bounded proof backend retains at most 256 journal entries. Do not
			// evict facts or turn evidence beyond this bound into replay rights.
			break
		}
		if err := ctx.Err(); err != nil {
			return restored, err
		}
		// Give the inspector detached values; it cannot mutate owner state.
		evidence := detachedReceiptOperation(prior)
		inspection, err := inspector.InspectCanonicalReceipt(ctx, evidence)
		if ctx.Err() != nil {
			return restored, ctx.Err()
		}
		if err != nil || inspection.State != operation.CanonicalReceiptTerminal || !canonicalReceiptIdentityMatches(prior, inspection.Operation) {
			// Inspection failures leave the original canonical result untouched.
			// Existing Coordinator quarantine/dependency checks govern safe work.
			continue
		}
		terminal := detachedReceiptOperation(inspection.Operation)
		if !canonicalReceiptEqual(prior, terminal) {
			// This is the same writer and mutex as normal Host execution. A save
			// failure, including unknown outcome, ends this owner before any ACK.
			if err := store.SaveOperation(ctx, s.ID, terminal); err != nil {
				return restored, err
			}
		}
		if backend, ok := s.runtime.Operations.(operation.CanonicalOperationAcknowledger); ok {
			if err := backend.AcknowledgeCanonicalOperation(ctx, detachedReceiptOperation(terminal)); err != nil {
				// The complete canonical save remains durable. Do not undo it or
				// turn an unconfirmed retention ACK into execution authorization.
				return restored, err
			}
		}
		// A previously terminal UNKNOWN can be absent from ResumeState's
		// unsettled set. Include its now-inspected terminal checkpoint so the
		// Coordinator replaces old uncertainty and builds typed next Context.
		if position, ok := positions[terminal.ID]; ok {
			restored.Operations[position] = terminal
		} else {
			positions[terminal.ID] = len(restored.Operations)
			restored.Operations = append(restored.Operations, terminal)
		}
	}
	return restored, nil
}

func detachedReceiptOperation(op operation.Operation) operation.Operation {
	op = operation.CloneExecutionMetadata(op)
	op.State = op.State.Clone()
	if op.Permission != nil {
		decision := *op.Permission
		op.Permission = &decision
	}
	if op.Denial != nil {
		denial := *op.Denial
		op.Denial = &denial
	}
	return op
}

func canonicalReceiptIdentityMatches(prior, terminal operation.Operation) bool {
	if prior.ID == "" || prior.ID != terminal.ID || prior.Type != terminal.Type || prior.Version != terminal.Version || prior.ToolName != terminal.ToolName || prior.MaxOutputLength != terminal.MaxOutputLength || len(prior.ExecutionBasis()) == 0 || !operation.SameExecutionMetadata(prior, terminal) {
		return false
	}
	if (prior.Permission == nil) != (terminal.Permission == nil) || prior.Permission != nil && *prior.Permission != *terminal.Permission {
		return false
	}
	if terminal.Status != operation.StatusCompleted && terminal.Status != operation.StatusFailed && terminal.Status != operation.StatusCanceled {
		return false
	}
	// P2 currently supports only immutable native RemoteJob plans. The backend
	// separately validates typed receipt semantics and realm-bound basis.
	a, errA := operation.DecodeRemoteJobState(prior)
	b, errB := operation.DecodeRemoteJobState(terminal)
	return errA == nil && errB == nil && a.Plan.Type == b.Plan.Type && a.Plan.Version == b.Plan.Version && bytes.Equal(a.Plan.Data, b.Plan.Data) && len(b.Handle) != 0 && b.Handle.IsValid()
}

func canonicalReceiptEqual(a, b operation.Operation) bool {
	return a.ID == b.ID && a.Type == b.Type && a.Version == b.Version && a.Status == b.Status && a.ToolName == b.ToolName && a.MaxOutputLength == b.MaxOutputLength && operation.SameExecutionMetadata(a, b) && bytes.Equal(a.State, b.State) && ((a.Permission == nil && b.Permission == nil) || (a.Permission != nil && b.Permission != nil && *a.Permission == *b.Permission)) && ((a.Denial == nil && b.Denial == nil) || (a.Denial != nil && b.Denial != nil && *a.Denial == *b.Denial))
}

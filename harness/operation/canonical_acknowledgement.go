package operation

import "context"

// ExecutionPreparationRequirement is an explicit execution-backend opt-in.
// It does not grant permission: the operation still passes the manager's
// authorization and the Coordinator's durable preparation acknowledgement.
// Ordinary local managers do not implement this interface.
type ExecutionPreparationRequirement interface {
	RequiresExecutionPreparation(Operation) bool
}

// CanonicalOperationAcknowledger receives terminal records only after the
// canonical Store has successfully saved them. An acknowledgement failure does
// not undo that save or authorize replay. A remote backend must reconcile its
// subordinate receipt with the same canonical operation after reconnecting.
type CanonicalOperationAcknowledger interface {
	AcknowledgeCanonicalOperation(context.Context, Operation) error
}

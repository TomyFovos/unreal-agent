package operation

import "context"

// CanonicalReceiptState is inspection-only evidence from an explicitly selected
// execution backend. Its zero value cannot grant execution or claim completion.
type CanonicalReceiptState uint8

const (
	CanonicalReceiptNotApplicable CanonicalReceiptState = iota
	CanonicalReceiptUnknown
	CanonicalReceiptTerminal
)

type CanonicalReceiptInspection struct {
	State     CanonicalReceiptState
	Operation Operation
}

// CanonicalReceiptInspector is an optional startup boundary for a Host that
// already owns the canonical Session writer. Inspect must not Prepare, Commit,
// obtain a new Permission lease or allocate a replacement Operation ID. It must
// bind the saved receipt to this backend's exact workspace/runtime/realm/mount
// epoch, Session, original plan and basis. Unknown or missing execution evidence
// remains Unknown; absence is never permission to replay an accepted operation.
// Ordinary local managers do not implement this interface.
type CanonicalReceiptInspector interface {
	InspectCanonicalReceipt(context.Context, Operation) (CanonicalReceiptInspection, error)
}

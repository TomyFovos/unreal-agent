package subagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
)

type ControlRequest struct {
	ParentID         session.ID
	ParentGeneration string
	OperationID      operation.ID
	ChildID          session.ID
	InputID          inbox.ID
	Text             string
}

func validateControl(owner *host.Session, r ControlRequest) (operation.Operation, error) {
	if owner == nil || owner.ID != r.ParentID || owner.Generation != r.ParentGeneration {
		return operation.Operation{}, host.ErrStaleGeneration
	}
	if r.InputID == "" || len(r.Text) > 32768 {
		return operation.Operation{}, fmt.Errorf("invalid child control")
	}
	view, err := owner.Inspect(0, 1)
	if err != nil {
		return operation.Operation{}, err
	}
	for _, op := range view.Operations {
		if op.ID != r.OperationID {
			continue
		}
		p, err := DecodePlan(op)
		if err == nil && p.Action == "start" && p.ParentID == owner.ID && p.ChildID == r.ChildID {
			return op, nil
		}
	}
	return operation.Operation{}, fmt.Errorf("child ownership mismatch")
}
func SteerChild(ctx context.Context, owner *host.Session, r ControlRequest) (host.Receipt, error) {
	if _, err := validateControl(owner, r); err != nil {
		return host.Receipt{}, err
	}
	spec, err := NewSpec(Plan{Version: 1, Action: "send", ParentID: owner.ID, Handle: r.OperationID, Text: r.Text})
	if err != nil {
		return host.Receipt{}, err
	}
	digest := sha256.Sum256([]byte(string(owner.ID) + "\x00" + string(r.InputID)))
	id := operation.ID("control-" + hex.EncodeToString(digest[:16]))
	return owner.SubmitOperation(ctx, r.ParentGeneration, r.InputID, id, "SubagentSend", spec)
}
func CancelChild(ctx context.Context, owner *host.Session, r ControlRequest) (host.Receipt, error) {
	if _, err := validateControl(owner, r); err != nil {
		return host.Receipt{}, err
	}
	return owner.CancelOperation(ctx, r.ParentGeneration, r.InputID, r.OperationID)
}

// ResumeChild validates the ownership boundary. A live parent automatically
// resumes all nonterminal children on Host.Resume, never by launching a new
// child from a Viewer connection. Committed terminal outcomes are immutable.
func ResumeChild(ctx context.Context, owner *host.Session, r ControlRequest) error {
	op, err := validateControl(owner, r)
	if err != nil {
		return err
	}
	if err = context.Cause(ctx); err != nil {
		return err
	}
	if op.Status == operation.StatusCompleted || op.Status == operation.StatusCanceled || op.Status == operation.StatusFailed {
		return fmt.Errorf("terminal child cannot be resumed")
	}
	// No dispatch is necessary: the owning Coordinator already owns this op.
	select {
	case <-owner.Done():
		return host.ErrStopped
	default:
		return nil
	}
}

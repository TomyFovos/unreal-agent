package viewer

import (
	"context"
	"fmt"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/subagent"
)

// SubagentOptions binds the real versioned Subagent plan and child Finish codec.
func SubagentOptions() Options {
	return Options{DecodeChild: DecodeSubagent, DecodeFinish: DecodeFinish}
}
func DecodeSubagent(parent session.ID, op operation.Operation) (Child, bool, error) {
	if op.Type != operation.TypeRemoteJob {
		return Child{}, false, nil
	}
	state, err := operation.DecodeRemoteJobState(op)
	if err != nil {
		return Child{}, false, err
	}
	if state.Plan.Type != subagent.PlanType {
		return Child{}, false, nil
	}
	plan, err := subagent.DecodePlan(op)
	if err != nil {
		return Child{}, false, err
	}
	if plan.ParentID != parent {
		return Child{}, false, fmt.Errorf("subagent parent identity mismatch")
	}
	if plan.Action != "start" {
		return Child{}, false, nil
	}
	return Child{ID: plan.ChildID, Label: short(plan.Text)}, true, nil
}
func DecodeFinish(item host.HistoryItem) (*Finish, error) {
	if item.Kind != sessionstore.ItemHostRecord {
		return nil, nil
	}
	if _, ok := item.Data.(host.ProjectInstructionRecord); ok {
		return nil, nil
	}
	record, ok := item.Data.(sessionstore.HostRecord)
	if !ok {
		return nil, fmt.Errorf("invalid canonical host record")
	}
	if record.Kind != "finish" {
		return nil, nil
	}
	if err := record.Validate(); err != nil {
		return nil, err
	}
	f := record.Finish
	return &Finish{OperationID: f.OperationID, Status: f.Result.Status, Summary: f.Result.Summary,
		ChangedFiles: append([]string(nil), f.Result.ChangedFiles...), Tests: append([]string(nil), f.Result.Tests...), Blockers: append([]string(nil), f.Result.Blockers...), RecordedAt: item.RecordedAt}, nil
}

// ParentReader is restricted to a Host-owned parent and its canonical direct
// children. Child reads never acquire writer ownership or imply runtime liveness.
type ParentReader struct {
	Host      *host.Host
	ParentID  session.ID
	Directory string
}

func (r ParentReader) owner(ctx context.Context) (*host.Session, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	if r.Host == nil || r.ParentID == "" {
		return nil, ErrUnavailable
	}
	return r.Host.Attach(r.ParentID)
}
func (r ParentReader) Inspect(ctx context.Context, id session.ID, after sessionstore.Sequence, limit int) (host.View, error) {
	if limit < 1 || limit > 4096 {
		return host.View{}, ErrInvalidPage
	}
	owner, err := r.owner(ctx)
	if err != nil {
		return host.View{}, err
	}
	if id == r.ParentID {
		return owner.Inspect(after, limit)
	}
	latest, err := owner.Inspect(0, 1)
	if err != nil {
		return host.View{}, err
	}
	var identity operation.ID
	for _, op := range latest.Operations {
		child, ok, err := DecodeSubagent(r.ParentID, op)
		if err != nil {
			return host.View{}, err
		}
		if ok && child.ID == id {
			if identity != "" {
				return host.View{}, fmt.Errorf("ambiguous child identity")
			}
			identity = op.ID
		}
	}
	if identity == "" {
		return host.View{}, ErrUnavailable
	}
	child, err := subagent.ReadChild(ctx, r.Directory, id, after, min(limit, 256))
	if err != nil {
		return host.View{}, err
	}
	config := child.Configuration
	if config == nil || config.ParentID != r.ParentID || config.ChildID != id || config.OperationID != identity {
		return host.View{}, fmt.Errorf("child canonical configuration does not match parent")
	}
	return child.View, nil
}
func (r ParentReader) Subscribe(ctx context.Context, id session.ID, after sessionstore.Sequence, limit, capacity int) (host.Subscription, error) {
	if capacity < 1 || capacity > 4096 {
		return host.Subscription{}, ErrInvalidPage
	}
	if id == r.ParentID {
		owner, err := r.owner(ctx)
		if err != nil {
			return host.Subscription{}, err
		}
		return owner.Subscribe(after, limit, capacity)
	}
	v, err := r.Inspect(ctx, id, after, limit)
	if err != nil {
		return host.Subscription{}, err
	}
	// A filesystem snapshot is not a live subscription. The UI may explicitly
	// Refresh selected children; it must continue to show runtime unknown.
	closed := make(chan host.Event)
	close(closed)
	return host.Subscription{Initial: v, Events: closed, Cancel: func() {}}, nil
}

// HostControls delegates every action to the existing owner. A Viewer cannot
// open/resume a stopped parent; explicit gateway Open uses the Host's lock gate.
type HostControls struct{ Host *host.Host }

func (c HostControls) owner(ctx context.Context, r ControlRequest) (*host.Session, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	if c.Host == nil {
		return nil, ErrUnavailable
	}
	return c.Host.Attach(r.ParentID)
}
func (c HostControls) SteerChild(ctx context.Context, r ControlRequest) (host.Receipt, error) {
	owner, err := c.owner(ctx, r)
	if err != nil {
		return host.Receipt{}, err
	}
	return subagent.SteerChild(ctx, owner, subagent.ControlRequest(r))
}
func (c HostControls) CancelChild(ctx context.Context, r ControlRequest) (host.Receipt, error) {
	owner, err := c.owner(ctx, r)
	if err != nil {
		return host.Receipt{}, err
	}
	return subagent.CancelChild(ctx, owner, subagent.ControlRequest(r))
}
func (c HostControls) ResumeChild(ctx context.Context, r ControlRequest) error {
	owner, err := c.owner(ctx, r)
	if err != nil {
		return err
	}
	return subagent.ResumeChild(ctx, owner, subagent.ControlRequest(r))
}

var _ Reader = ParentReader{}
var _ Controls = HostControls{}

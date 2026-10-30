package subagent

import (
	"context"
	"fmt"

	"github.com/unreallabsai/unreal-agent/harness/operation"
)

func terminalStatus(status operation.Status) bool {
	return status == operation.StatusCompleted || status == operation.StatusFailed || status == operation.StatusCanceled
}

// EndedMessage is an acknowledged control result, not a transport failure.
func EndedMessage(handle operation.ID, status operation.Status) string {
	return fmt.Sprintf("Child %s has ended (status: %s). Results: subagent show %s", handle, status, handle)
}

type childEnded struct {
	handle operation.ID
	status operation.Status
}

func (e childEnded) Error() string { return EndedMessage(e.handle, e.status) }

func (m *Manager) closedChildStatus(ctx context.Context, id operation.ID) operation.Status {
	m.mu.Lock()
	j := m.jobs[id]
	m.mu.Unlock()
	if j != nil {
		select {
		case <-j.cancel:
			return operation.StatusCanceled
		default:
		}
	}
	view, err := m.config.Owner.Inspect(0, 1)
	if err == nil {
		for _, op := range view.Operations {
			if op.ID == id && terminalStatus(op.Status) {
				return op.Status
			}
		}
	}
	// The channel can close just before its parent's terminal update commits.
	child, err := ReadChildSnapshot(ctx, m.config.Directory, ChildID(m.config.Owner.ID, id))
	if err == nil && child.Finish != nil {
		if child.Finish.Result.Status == "completed" {
			return operation.StatusCompleted
		}
		return operation.StatusFailed
	}
	return operation.StatusFailed
}

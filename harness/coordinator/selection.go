package coordinator

import (
	"context"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
)

func (current *coordinator) pendingRuntimeChange() bool {
	if current.dependencies.RuntimeSelection == nil {
		return false
	}
	active, pending := current.dependencies.RuntimeSelection()
	return active != nil && pending != nil && (active.Provider != pending.Provider || active.Model != pending.Model ||
		active.Effort.Normalize() != pending.Effort.Normalize() || active.Fast != pending.Fast)
}

func (current *coordinator) resumeUsageWaitForSelection(ctx context.Context) error {
	if current.usageRetryAt.IsZero() || current.dependencies.RuntimeSelection == nil || current.stop.request.Mode == inbox.StopHard || current.waitingForOwnToolBatch() {
		return nil
	}
	active, pending := current.dependencies.RuntimeSelection()
	if active == nil || pending == nil || active.Provider == pending.Provider && active.Model == pending.Model {
		return nil
	}
	if err := current.clearUsageWait(ctx); err != nil {
		return err
	}
	current.modelRetry = nil
	current.state.callModel = true
	return nil
}

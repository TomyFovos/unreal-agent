package host

import (
	"context"
	"errors"
	"io/fs"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
)

func unfinishedChild(op operation.Operation) bool {
	return op.Type == operation.TypeRemoteJob && op.ToolName == "SubagentStart" &&
		op.Status != operation.StatusCompleted && op.Status != operation.StatusFailed && op.Status != operation.StatusCanceled
}

// cancelStoppedChildren runs after the runtime has drained its processes and
// Operation manager. No executor can overwrite these final checkpoints.
func (s *Session) cancelStoppedChildren(ctx context.Context, interrupted bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result error
	for _, op := range s.operations {
		if !unfinishedChild(op) {
			continue
		}
		if interrupted && s.runtime.InterruptedInput != nil {
			input, err := s.runtime.InterruptedInput(op)
			if err == nil && input != nil && s.submissions[input.ID] == nil {
				err = s.store.AppendInput(ctx, s.ID, *input)
			}
			result = errors.Join(result, err)
		}
		step, err := operation.CancelRemoteJob(op)
		if err == nil {
			err = s.store.SaveOperation(ctx, s.ID, *step.Operation)
		}
		if err == nil {
			s.setOperation(*step.Operation)
			s.broadcast(Event{Kind: "operation", Operation: step.Operation})
		}
		result = errors.Join(result, err)
	}
	return result
}

// reconcileStoppedSessions runs once during this Host's initial restoration.
// Writer ownership excludes live Sessions, including parents in other processes.
// It only checkpoints abandoned children; it never builds or resumes a runtime.
func (h *Host) reconcileStoppedSessions(ctx context.Context, owner *Session) error {
	store, err := localfile.New(h.config.Directory)
	if err != nil {
		return err
	}
	sessions, err := store.ListSessions(ctx)
	if err != nil {
		return err
	}
	for _, info := range sessions {
		if info.ID == owner.ID {
			continue // Coordinator restores this Session's operations and notices.
		}
		lock, err := store.AcquireWriter(info.ID)
		if errors.Is(err, localfile.ErrWriterOwned) {
			continue
		}
		if err != nil {
			return err
		}
		// Most stopped children have no children of their own. Decide that
		// from Operation metadata without decoding or restoring old responses.
		ops, scanErr := store.RecoveryOperations(ctx, info.ID)
		err = scanErr
		if err == nil {
			for _, op := range ops {
				if unfinishedChild(op) {
					err = reconcileStoppedChildren(ctx, store, info.ID, owner.runtime.InterruptedInput)
					break
				}
			}
		}
		err = errors.Join(err, lock.Close())
		if err != nil {
			return err
		}
	}
	return nil
}

func reconcileStoppedChildren(ctx context.Context, store *localfile.Store, id session.ID, notice func(operation.Operation) (*inbox.Input, error)) error {
	_, items, ops, err := store.ReadSnapshot(ctx, id)
	if err != nil {
		return err
	}
	seen := map[inbox.ID]bool{}
	for _, item := range items {
		if input, ok := item.Data.(inbox.Input); ok {
			seen[input.ID] = true
		}
	}
	for _, op := range ops {
		if !unfinishedChild(op) {
			continue
		}
		if notice != nil {
			input, err := notice(op)
			if err != nil {
				return err
			}
			if input != nil && !seen[input.ID] {
				if err = store.AppendInput(ctx, id, *input); err != nil {
					return err
				}
				seen[input.ID] = true
			}
		}
		step, err := operation.CancelRemoteJob(op)
		if err != nil {
			return err
		}
		if err = store.SaveOperation(ctx, id, *step.Operation); err != nil {
			return err
		}
	}
	return nil
}

// ReconcileStoppedSession settles children of a reaped child process, including
// a process that died before its own Host could checkpoint its shutdown.
func ReconcileStoppedSession(ctx context.Context, directory string, id session.ID) error {
	store, err := localfile.New(directory)
	if err != nil {
		return err
	}
	lock, err := store.AcquireWriter(id)
	if errors.Is(err, localfile.ErrWriterOwned) {
		return nil // Another Host has already resumed this Session.
	}
	if err != nil {
		return err
	}
	err = reconcileStoppedChildren(ctx, store, id, nil)
	if errors.Is(err, fs.ErrNotExist) {
		err = nil // A failed process start need not have created a Session.
	}
	return errors.Join(err, lock.Close())
}

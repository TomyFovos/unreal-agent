package host

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	"github.com/unreallabsai/unreal-agent/harness/subagent/reporting"
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
		if interrupted && s.runtime.RetainOperation != nil {
			live, err := s.runtime.RetainOperation(ctx, op)
			if err != nil {
				result = errors.Join(result, err)
				continue
			}
			if live {
				continue
			}
		}
		if interrupted && s.runtime.InterruptedInput != nil && !s.childReports.Has(op.ID) {
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
	var parents []session.ID
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
					err = reconcileStoppedChildren(ctx, store, info.ID, owner.runtime.InterruptedInput, owner.runtime.RetainOperation)
					break
				}
			}
		}
		if err == nil {
			for _, op := range ops {
				if op.Type == operation.TypeRemoteJob && op.ToolName == "SubagentStart" {
					parents = append(parents, info.ID)
					break
				}
			}
		}
		err = errors.Join(err, lock.Close())
		if err != nil {
			return err
		}
	}
	// Reconcile the entire live set before moving anything from that snapshot.
	for _, id := range parents {
		lock, err := store.AcquireWriter(id)
		if errors.Is(err, localfile.ErrWriterOwned) {
			continue
		}
		if err != nil {
			return err
		}
		retry, archiveErr := archiveStoppedParent(ctx, store, id)
		if archiveErr != nil {
			slog.WarnContext(ctx, "archive stopped children", "session", id, "error", archiveErr)
		}
		if err := lock.Close(); err != nil {
			return err
		}
		if retry {
			h.retryStoppedArchive(id)
		}
	}
	return nil
}

func reconcileStoppedChildren(ctx context.Context, store *localfile.Store, id session.ID, notice func(operation.Operation) (*inbox.Input, error), retain func(context.Context, operation.Operation) (bool, error)) error {
	_, items, ops, err := store.ReadSnapshot(ctx, id)
	if err != nil {
		return err
	}
	seen := map[inbox.ID]bool{}
	var reports reporting.Index
	for _, item := range items {
		reports.ObserveItem(item)
		if input, ok := item.Data.(inbox.Input); ok {
			seen[input.ID] = true
		}
	}
	for _, op := range ops {
		reports.ObserveOperation(op)
	}
	for _, op := range ops {
		if !unfinishedChild(op) {
			continue
		}
		if retain != nil {
			live, err := retain(ctx, op)
			if err != nil {
				return err
			}
			if live {
				continue
			}
		}
		if notice != nil && !reports.Has(op.ID) {
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
	err = reconcileStoppedChildren(ctx, store, id, nil, nil)
	if errors.Is(err, fs.ErrNotExist) {
		err = nil // A failed process start need not have created a Session.
	}
	return errors.Join(err, lock.Close())
}

package subagent

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io/fs"
	"os"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
)

// RetainOperation checks the child's own executor, independently of the parent
// Host's lifetime. A writer lease survives reconnects and is released by the OS
// when its process dies. Never infer death merely from missing Finish or age.
func (m *Manager) RetainOperation(ctx context.Context, op operation.Operation) (bool, error) {
	p, err := DecodePlan(op)
	if err != nil || p.Action != "start" {
		return false, nil
	}
	store, err := localfile.New(m.config.Directory)
	if err != nil {
		return false, err
	}
	// Settled children have no executor or pending parent delivery to recover.
	// Restore uses their location index, never the archived transcript. History
	// readers still open the untouched canonical record explicitly by ID.
	if terminalStatus(op.Status) {
		entry, err := store.ArchiveIndex(p.ChildID)
		if err == nil && !entry.Pending && entry.ParentID == p.ParentID && entry.OperationID == op.ID {
			return false, nil
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return false, err
		}
	}
	lock, err := store.AcquireWriter(p.ChildID)
	if errors.Is(err, localfile.ErrWriterOwned) {
		if p.ParentID == m.config.Owner.ID && !terminalStatus(op.Status) {
			m.mu.Lock()
			m.recovering[op.ID] = true
			m.mu.Unlock()
		}
		return !terminalStatus(op.Status), nil
	}
	if err != nil {
		return false, err
	}
	defer lock.Close()
	_, err = m.recordOrphanEnd(ctx, store, op.ID, p)
	return false, err
}

// Caller holds the child's writer lease. This only appends evidence of a lost
// executor; it never resumes work or fabricates a Finish result.
func (m *Manager) recordOrphanEnd(ctx context.Context, store *localfile.Store, id operation.ID, p Plan) (*ChildView, error) {
	view, err := ReadChildSnapshot(ctx, m.config.Directory, p.ChildID)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if view.Configuration != nil && (view.Configuration.ParentID != p.ParentID || view.Configuration.OperationID != id) {
		return &view, nil
	}
	if view.Finish == nil && view.End == nil {
		view.End = &sessionstore.EndRecord{EndedBy: "host_restart", EndedAt: view.LastRecordedAt}
		if err := store.AppendHostRecord(ctx, p.ChildID, sessionstore.HostRecord{Version: 1, Kind: sessionstore.HostEnded, End: view.End}); err != nil {
			return nil, err
		}
	}
	if view.End != nil {
		m.mu.Lock()
		m.restartEnds[id] = view.End
		m.mu.Unlock()
	}
	return &view, nil
}

// A recovered live child owns its existing runtime. Observe its committed
// completion while it reconnects; never spawn a replacement for the same ID.
func (m *Manager) watchRecovered(ctx context.Context, id operation.ID, p Plan, state *operation.RemoteJobState) (operation.Status, error) {
	store, err := localfile.New(m.config.Directory)
	if err != nil {
		return operation.StatusFailed, err
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var previous os.FileInfo
	var view ChildView
	finished := func(view ChildView) (operation.Status, error) {
		handle := Handle{Version: 1, ParentID: p.ParentID, ChildID: p.ChildID, OperationID: id, Finish: view.Finish, FinishLocator: view.FinishLocator}
		if view.Configuration != nil {
			handle.Brief = view.Configuration.Brief
		}
		var err error
		state.Handle, err = json.Marshal(handle)
		state.TerminalResult = view.Finish.Result.Summary
		return finishStatus(handle), err
	}
	for {
		info, err := store.StatSession(p.ChildID)
		if err != nil {
			return operation.StatusFailed, err
		}
		if previous == nil || !os.SameFile(previous, info) || previous.Size() != info.Size() || previous.ModTime() != info.ModTime() {
			view, err = ReadChildSnapshot(ctx, m.config.Directory, p.ChildID)
			if err != nil {
				return operation.StatusFailed, err
			}
			previous = info
		}
		if view.Finish != nil {
			return finished(view)
		}
		lock, err := store.AcquireWriter(p.ChildID)
		if err == nil {
			view, recordErr := m.recordOrphanEnd(ctx, store, id, p)
			err = recordErr
			err = errors.Join(err, lock.Close())
			if err == nil && view != nil && view.Finish != nil {
				return finished(*view)
			}
			return operation.StatusCanceled, err
		}
		if !errors.Is(err, localfile.ErrWriterOwned) {
			return operation.StatusFailed, err
		}
		select {
		case <-ctx.Done():
			return operation.StatusCanceled, context.Cause(ctx)
		case <-ticker.C:
		}
	}
}

package host

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io/fs"
	"log/slog"
	"strings"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	"github.com/unreallabsai/unreal-agent/harness/subagent/identity"
	"github.com/unreallabsai/unreal-agent/harness/subagent/reporting"
)

// This projection deliberately contains only canonical lineage and control
// fields. Host owns storage lifecycle; subagent owns execution and full plans.
type archivePlan struct {
	Version  uint32
	Action   string
	ParentID session.ID
	ChildID  session.ID
	Handle   operation.ID
	Request  string
}

func childArchivePlan(op operation.Operation) (archivePlan, bool) {
	state, err := operation.DecodeRemoteJobState(op)
	var p archivePlan
	ok := err == nil && state.Plan.Type == "subagent" && state.Plan.Version == 1 && json.Unmarshal(state.Plan.Data, &p) == nil && p.Version == 1
	return p, ok
}

// Parent messages carry a canonical kind, never a prose heuristic. A decision
// message remains unanswered until a reply is committed in that child. Approval
// requests are tracked by the start handle's canonical Approvals instead.
// Replay uses the first durable answer intent and the child's receipt, so an old
// receipt cannot answer a later question merely by appearing later on disk.
func unansweredChildren(items []sessionstore.Item, ops map[operation.ID]operation.Operation) map[operation.ID][]operation.ID {
	pending := make(map[operation.ID][]operation.ID)
	seen := make(map[operation.ID]bool)
	answer := func(op operation.Operation) {
		if seen[op.ID] {
			return
		}
		seen[op.ID] = true
		if _, ok := ops[op.ID]; !ok {
			return
		}
		p, ok := childArchivePlan(op)
		if ok && p.Action == "send" {
			if _, waiting := pending[p.Handle]; waiting {
				pending[p.Handle] = append(pending[p.Handle], op.ID)
			}
		}
	}
	for _, item := range items {
		switch value := item.Data.(type) {
		case inbox.Input:
			if peer, err := value.DecodePeerMessage(); err == nil && peer.Kind == "message" && !strings.HasPrefix(string(value.ID), "approval:") && peer.Sender == string(identity.ChildID(session.ID(peer.Target), operation.ID(peer.Handle))) {
				pending[operation.ID(peer.Handle)] = nil
			}
			if op, err := sessionstore.DecodeOperationIntent(value); err == nil {
				answer(op)
			}
		case sessionstore.ToolCallStatus:
			for _, op := range value.Operations {
				answer(op)
			}
		case sessionstore.Fork:
			clear(pending)
			clear(seen)
		}
	}
	return pending
}

// Caller holds the parent's writer ownership and stable canonical projection.
// retry is true only for eligible children whose executor has not drained yet.
func archiveSettledChildren(ctx context.Context, store *localfile.Store, parent session.ID, items []sessionstore.Item, ops map[operation.ID]operation.Operation, reports *reporting.Index, archived map[operation.ID]bool) (retry bool, result error) {
	var pending map[operation.ID][]operation.ID
	for _, op := range ops {
		if archived[op.ID] || !terminal(op.Status) || !reports.Has(op.ID) {
			continue
		}
		p, ok := childArchivePlan(op)
		if !ok || p.Action != "start" || p.ParentID != parent {
			continue
		}
		child := identity.ChildID(parent, op.ID)
		if p.ChildID != "" && p.ChildID != child {
			continue
		}
		if entry, err := store.ArchiveIndex(child); err == nil && !entry.Pending {
			archived[op.ID] = true
			continue
		} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
			result = errors.Join(result, err)
			continue
		}
		state, _ := operation.DecodeRemoteJobState(op)
		var handle struct{ Approvals []struct{ ID string } }
		if len(state.Handle) > 0 && json.Unmarshal(state.Handle, &handle) != nil {
			continue
		}
		if len(handle.Approvals) > 0 {
			unanswered := make(map[string]bool, len(handle.Approvals))
			for _, approval := range handle.Approvals {
				unanswered[approval.ID] = true
			}
			for _, response := range ops {
				p, ok := childArchivePlan(response)
				if ok && response.Status == operation.StatusCompleted && p.Action == "approve" && p.ParentID == parent && p.Handle == op.ID {
					delete(unanswered, p.Request)
				}
			}
			if len(unanswered) > 0 {
				continue
			}
		}
		if pending == nil {
			pending = unansweredChildren(items, ops)
		}
		if answers, waiting := pending[op.ID]; waiting {
			if len(answers) == 0 {
				continue
			}
			_, childItems, _, err := store.ReadSnapshot(ctx, child)
			if err != nil {
				result = errors.Join(result, err)
				continue
			}
			delivered := false
			for _, item := range childItems {
				input, ok := item.Data.(inbox.Input)
				if !ok {
					continue
				}
				peer, err := input.DecodePeerMessage()
				if err != nil || peer.Kind != "message" || peer.Sender != string(parent) || peer.Target != string(child) || peer.Handle != string(op.ID) {
					continue
				}
				for _, answer := range answers {
					delivered = delivered || input.ID == inbox.ID("send:"+string(answer))
				}
			}
			if !delivered {
				continue
			}
		}
		moved, err := store.ArchiveChild(ctx, localfile.ArchiveEntry{ChildID: child, ParentID: parent, OperationID: op.ID, Status: op.Status})
		if errors.Is(err, fs.ErrNotExist) {
			continue // A rejected/failed start can have no child record.
		}
		result = errors.Join(result, err)
		if moved {
			archived[op.ID] = true
		} else if err == nil {
			retry = true
		}
	}
	return retry, result
}

func (s *Session) wakeChildArchive() {
	select {
	case s.archiveWake <- struct{}{}:
	default:
	}
}

func (s *Session) archiveChildren(ctx context.Context) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.archivedChildren == nil {
		s.archivedChildren = make(map[operation.ID]bool)
	}
	retry, err := archiveSettledChildren(ctx, s.store, s.ID, s.items, s.operations, &s.childReports, s.archivedChildren)
	if err != nil && ctx.Err() == nil {
		slog.WarnContext(ctx, "archive settled children", "session", s.ID, "error", err)
	}
	return retry
}

// Coalesce canonical changes. A bounded observation interval is only for lease
// release (not an age threshold); no eligible child waits for another restart.
func (s *Session) archiveLoop(ctx context.Context) {
	var retry <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.archiveWake:
		case <-retry:
		}
		retry = nil
		if s.archiveChildren(ctx) {
			retry = time.After(time.Second)
		}
	}
}

func archiveStoppedChildren(ctx context.Context, store *localfile.Store, parent session.ID) error {
	_, err := archiveStoppedParent(ctx, store, parent)
	return err
}

func archiveStoppedParent(ctx context.Context, store *localfile.Store, parent session.ID) (bool, error) {
	_, items, operations, err := store.ReadSnapshot(ctx, parent)
	if err != nil {
		return false, err
	}
	var reports reporting.Index
	for _, item := range items {
		reports.ObserveItem(item)
	}
	ops := make(map[operation.ID]operation.Operation, len(operations))
	for _, op := range operations {
		ops[op.ID] = op
		reports.ObserveOperation(op)
	}
	return archiveSettledChildren(ctx, store, parent, items, ops, &reports, make(map[operation.ID]bool))
}

// A stopped parent has no Session loop to observe its child's lease release.
// Retry only parents with otherwise eligible children; do not scan the archive
// or resume a stopped runtime. Reacquire ownership before each canonical read.
func (h *Host) retryStoppedArchive(parent session.ID) {
	h.maintenance.start(func(ctx context.Context) {
		store, err := localfile.New(h.config.Directory)
		if err != nil {
			slog.WarnContext(ctx, "open stopped child archive", "session", parent, "error", err)
			return
		}
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			lease, err := store.AcquireWriter(parent)
			if errors.Is(err, localfile.ErrWriterOwned) {
				continue
			}
			if err != nil {
				slog.WarnContext(ctx, "own stopped child archive", "session", parent, "error", err)
				return
			}
			retry, err := archiveStoppedParent(ctx, store, parent)
			err = errors.Join(err, lease.Close())
			if err != nil && ctx.Err() == nil {
				slog.WarnContext(ctx, "archive stopped children", "session", parent, "error", err)
			}
			if !retry || err != nil {
				return
			}
		}
	})
}

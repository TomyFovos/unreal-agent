package subagent

import (
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"golang.org/x/sys/unix"
)

type activeChildLimit struct {
	mu      sync.Mutex
	current int
}

func (m *Manager) maxActiveChildren() int {
	limit := &m.limit
	limit.mu.Lock()
	defer limit.mu.Unlock()
	if m.config.ContextFile != nil {
		data, err := os.ReadFile(m.config.ContextFile.Path())
		var doc struct{ MaxActiveChildren int }
		if err == nil && json.Unmarshal(data, &doc) == nil && doc.MaxActiveChildren >= 0 {
			limit.current = doc.MaxActiveChildren
		}
	}
	return limit.current
}

func (m *Manager) treeRoot(ctx context.Context) session.ID {
	if m.config.TreeRoot != "" {
		return m.config.TreeRoot
	}
	if m.config.Child == nil || m.usage == nil {
		return m.config.Owner.ID
	}
	m.usage.mu.Lock()
	defer m.usage.mu.Unlock()
	id := m.config.Child.ParentID
	for seen := map[session.ID]bool{}; !seen[id]; {
		seen[id] = true
		node, err := m.usage.readNode(ctx, id)
		if err != nil || node.Parent == "" {
			return id
		}
		id = node.Parent
	}
	return id
}

func (r *UsageReader) isQueued(ctx context.Context, parent session.ID, id operation.ID) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, err := r.readNode(ctx, parent)
	return err == nil && n.queued[id] != 0 && !n.started[id], err
}

func (m *Manager) wasCanceled(j *job) bool {
	select {
	case <-j.cancel:
		return true
	default:
		return false
	}
}

func (m *Manager) publishStart(j *job, handle Handle, state *operation.RemoteJobState) {
	state.Handle, _ = json.Marshal(handle)
	checkpoint, _ := operation.UpdateRemoteJob(j.operation, *state, operation.StatusAwaiting)
	m.emit(*checkpoint.Operation)
}

type queuePosition struct {
	active, ahead int
	order, last   uint64
}

// One local lock serializes admission across parent and child processes.
// Queue tickets and reservations are records in the owning Session's log.
// Losing the process drops the lock, leaving those decisions available to restore.
func (m *Manager) admitChild(ctx context.Context, j *job, p Plan, started llm.ActivityRecord, handle *Handle, state *operation.RemoteJobState) error {
	queued, _ := m.usage.isQueued(ctx, p.ParentID, j.operation.ID)
	if m.maxActiveChildren() == 0 && !queued && m.config.ContextFile == nil {
		if err := m.config.Owner.RecordActivity(ctx, started); err != nil {
			slog.Warn("record child startup", "error", err)
		}
		return nil
	}
	root := m.treeRoot(ctx)
	key := sha256.Sum256([]byte(root))
	path := filepath.Join(m.config.Directory, fmt.Sprintf(".child-admission-%x.lock", key[:16]))
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		lockErr := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if lockErr == nil {
			var ahead *int
			err = func() error {
				defer unix.Flock(fd, unix.LOCK_UN)
				maximum := m.maxActiveChildren()
				pos, err := m.queuePosition(ctx, root, p.ParentID, j.operation.ID)
				if err != nil {
					return err
				}
				if (maximum == 0 || pos.active < maximum) && pos.ahead == 0 {
					started.StartedAt = time.Now().UTC()
					return m.config.Owner.RecordActivity(ctx, started)
				}
				if pos.order == 0 {
					if err := m.config.Owner.RecordActivity(ctx, llm.ActivityRecord{Kind: childQueued, ID: string(j.operation.ID), QueueOrder: pos.last + 1}); err != nil {
						return err
					}
				}
				ahead = new(pos.ahead)
				return nil
			}()
			if err != nil || ahead == nil {
				return err
			}
			if handle.QueuedAhead == nil {
				handle.QueuedAhead = ahead
				m.publishStart(j, *handle, state)
			}
		} else if !errors.Is(lockErr, unix.EWOULDBLOCK) && !errors.Is(lockErr, unix.EINTR) {
			return lockErr
		}
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-ticker.C:
		}
	}
}

func (m *Manager) queuePosition(ctx context.Context, root, parent session.ID, id operation.ID) (queuePosition, error) {
	r := m.usage
	r.mu.Lock()
	defer r.mu.Unlock()
	entries, err := r.readTree(ctx, root, true)
	if err != nil {
		return queuePosition{}, err
	}
	var pos queuePosition
	if n := r.nodes[parent]; n != nil {
		pos.order = n.queued[id]
	}
	for _, entry := range entries {
		if entry.node == nil {
			continue
		}
		for handle, child := range entry.node.children {
			if terminalStatus(child.status) {
				continue
			}
			if entry.node.started[handle] {
				pos.active++
			} else if child.status != operation.StatusCanceling {
				ticket := entry.node.queued[handle]
				pos.last = max(pos.last, ticket)
				if ticket != 0 && (pos.order == 0 || ticket < pos.order) {
					pos.ahead++
				}
			}
		}
	}
	return pos, nil
}

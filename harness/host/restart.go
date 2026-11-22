package host

import (
	"context"

	"github.com/unreallabsai/unreal-agent/harness/operation"
)

// Workload counts active Sessions and their nonterminal work. Child starts
// count as children; Operations counts the other work owned by this Host.
type Workload struct {
	ActiveSessions int
	Operations     int
	Children       int
}

func (s *Session) setCoordinatorIdle(idle bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.coordinatorIdle = idle
	return s.ctx.Err() == nil
}

func (h *Host) Workload() Workload {
	work, _ := h.workload(context.Background(), false, nil)
	return work
}

// Stopping reports whether the Host has closed admission and is draining.
func (h *Host) Stopping() bool { return h.ctx.Err() != nil }

// PrepareRestart closes admission and cancels the Host in the same critical
// section as the workload check. With no confirmed counts, only an idle Host
// can accept a restart. The caller drains the Host with Close before handing
// its socket and Session writers to a replacement.
func (h *Host) PrepareRestart(ctx context.Context, confirmed *Workload) (Workload, bool) {
	return h.workload(ctx, true, confirmed)
}

func (h *Host) workload(ctx context.Context, restart bool, confirmed *Workload) (Workload, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	// Hold Session admission and coordinator boundaries together across the
	// check. A submission that won its lock is counted even before its receipt.
	for _, s := range h.sessions {
		s.mu.Lock()
	}
	defer func() {
		for _, s := range h.sessions {
			s.mu.Unlock()
		}
	}()
	var work Workload
	for _, s := range h.sessions {
		if s.running && (!s.coordinatorIdle || s.pendingSubmissions != 0 || s.responding > 0) {
			work.ActiveSessions++
		}
		for _, op := range s.operations {
			switch op.Status {
			case operation.StatusCompleted, operation.StatusFailed, operation.StatusCanceled:
				continue
			}
			if unfinishedChild(op) {
				work.Children++
			} else {
				work.Operations++
			}
		}
	}
	wanted := Workload{}
	if confirmed != nil {
		wanted = *confirmed
	}
	if !restart || work != wanted || h.closed || ctx.Err() != nil {
		return work, false
	}
	h.closed = true
	h.cancel()
	return work, true
}

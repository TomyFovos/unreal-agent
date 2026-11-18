package host

import (
	"context"
	"log/slog"

	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/internal/secretguard"
)

func (s *Session) recordApproval(approval permission.Approval) {
	approval.Request.Scope = secretguard.RedactSensitive(approval.Request.Scope)
	r := sessionstore.HostRecord{Version: 1, Kind: sessionstore.HostApproval, Approval: &approval}
	if err := (&serializedStore{s}).AppendHostRecord(context.WithoutCancel(s.ctx), s.ID, r); err != nil {
		slog.Warn("record permission approval", "session", s.ID, "error", err)
	}
}

// PermissionControl is reachable through the authenticated local control
// channel, never through Inbox, chat, tool arguments or restored history.
func (s *Session) PermissionControl(generation string) (*permission.Controller, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation != s.Generation {
		return nil, ErrStaleGeneration
	}
	if !s.running {
		return nil, ErrStopped
	}
	if s.permissions == nil {
		return nil, permission.ErrControl
	}
	return s.permissions, nil
}

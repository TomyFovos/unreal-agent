package host

import (
	"context"

	"github.com/unreallabsai/unreal-agent/harness/contextstate"
	"github.com/unreallabsai/unreal-agent/harness/record"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

// AppendContextEvent records an explicit L2 update with the Session's writer.
func (s *Session) AppendContextEvent(ctx context.Context, event contextstate.Event) (record.Locator, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running {
		return "", ErrStopped
	}
	return s.store.AppendContextEvent(ctx, s.ID, event)
}

func (s *Session) ContextState(ctx context.Context) (contextstate.State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.store.ContextState(ctx, s.ID)
}

func (s *Session) windowContextState(ctx context.Context) (contextstate.State, sessionstore.Sequence, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var sequence sessionstore.Sequence
	if len(s.items) > 0 {
		sequence = s.items[len(s.items)-1].Sequence
	}
	state, err := s.store.ContextState(ctx, s.ID)
	if state.Session.Started.IsZero() {
		state.Session.Started = s.snapshot.Session.CreatedAt
	}
	if s.workspace != "" {
		state.Session.Directory = s.workspace
	}
	if s.permissions != nil {
		state.Session.PermissionMode = string(s.permissions.Snapshot().Mode)
	}
	return state, sequence, err
}

package host

import (
	"context"

	"github.com/unreallabsai/unreal-agent/harness/contextstate"
	"github.com/unreallabsai/unreal-agent/harness/record"
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

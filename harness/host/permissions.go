package host

import "github.com/unreallabsai/unreal-agent/harness/permission"

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

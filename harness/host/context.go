package host

import "github.com/unreallabsai/unreal-agent/harness/contextengine"

// Context diagnostics are disposable request projections, like progress. They
// never go through Session Store and their loss cannot corrupt canonical state.
func (s *Session) contextBuilt(d contextengine.Diagnostics) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.contextReport = &d
	s.broadcast(Event{Kind: "context"})
}

package agentrunner

import (
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"sync"
)

// One history codec/Operation manager survives provider changes. Only available
// schemas and new-call translation follow the applied runtime capability.
type selectedTools struct {
	tool.Registry
	mu      sync.RWMutex
	enabled bool
	allowed func(string) bool
}

func (s *selectedTools) set(enabled bool, allowed func(string) bool) {
	s.mu.Lock()
	s.enabled, s.allowed = enabled, allowed
	s.mu.Unlock()
}
func (s *selectedTools) StaticDefinitions() []tool.Definition {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.enabled {
		return nil
	}
	var out []tool.Definition
	for _, d := range s.Registry.StaticDefinitions() {
		if s.allowed == nil || s.allowed(d.Tool.Name) {
			out = append(out, d)
		}
	}
	return out
}
func (s *selectedTools) Resolve(name string) (tool.Translator, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.enabled || s.allowed != nil && !s.allowed(name) {
		return nil, false
	}
	return s.Registry.Resolve(name)
}
func (s *selectedTools) ResolveHistory(name string) (tool.Translator, bool) {
	return tool.ResolveHistory(s.Registry, name)
}
func (s *selectedTools) Skills() []tool.Skill {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.enabled || s.allowed != nil && !s.allowed(tool.SkillUseName) {
		return nil
	}
	return s.Registry.Skills()
}

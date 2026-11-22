package llm

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
)

// ToolScopes indexes calls inside a tool's own conversation. Projections use
// it across responses so a call and its receipt inherit the same scope.
type ToolScopes struct {
	parents map[string]string
	counts  map[string]int
}

func (s *ToolScopes) note(callID, parent string) {
	if callID == "" || parent == "" || s.parents[callID] == parent {
		return
	}
	if s.parents == nil {
		s.parents = make(map[string]string)
		s.counts = make(map[string]int)
	}
	if old := s.parents[callID]; old != "" {
		s.counts[old]--
	}
	s.parents[callID] = parent
	s.counts[parent]++
}

func (s *ToolScopes) Observe(output []Item) {
	for _, item := range output {
		if call, ok := item.Data.(ToolCall); ok {
			s.note(call.CallID, item.ParentCallID)
		}
		for _, observation := range item.ToolObservations {
			if observation.Type == ItemToolCall || observation.Type == ItemToolResult {
				s.note(observation.CallID, observation.ParentCallID)
			}
		}
		if parent, body := legacyAgentMessage(item); parent != "" {
			var message struct {
				Content []struct {
					Type string `json:"type"`
					ID   string `json:"id"`
				} `json:"content"`
			}
			if json.Unmarshal([]byte(body), &message) == nil {
				for _, block := range message.Content {
					if block.Type == "tool_use" {
						s.note(block.ID, strings.Clone(parent))
					}
				}
			}
		}
	}
}

func (s *ToolScopes) Parent(callID string) string { return s.parents[callID] }

func (s *ToolScopes) Count(callID string) int { return s.counts[callID] }

func (s *ToolScopes) Nested(item Item) bool {
	if item.ParentCallID != "" || LegacyAgentMessage(item) {
		return true
	}
	switch v := item.Data.(type) {
	case ToolCall:
		return s.Parent(v.CallID) != ""
	case ToolResult:
		return s.Parent(v.CallID) != ""
	}
	return false
}

// LegacyAgentMessage recognizes Claude Code's old nested transcript envelope.
func LegacyAgentMessage(item Item) bool {
	parent, _ := legacyAgentMessage(item)
	return parent != ""
}

func legacyAgentMessage(item Item) (parent, body string) {
	m, ok := item.Data.(Message)
	if !ok || item.Type != ItemMessage || m.Role != RoleAssistant && m.Role != "" {
		return "", ""
	}
	const prefix = "[Claude Code Agent "
	if !strings.HasPrefix(m.Text, prefix) {
		return "", ""
	}
	parent, body, ok = strings.Cut(m.Text[len(prefix):], "] ")
	if !ok || parent == "" || strings.ContainsAny(parent, " \r\n\t") || !strings.HasPrefix(body, "{") {
		return "", ""
	}
	if strings.HasPrefix(item.ProviderID, "claude-code/") && strings.Contains(item.ProviderID, "/native/") {
		return parent, body
	}
	// Reduced display projections may no longer carry the provider ID.
	if item.ProviderID == "" && strings.HasPrefix(parent, "toolu_") {
		var message struct {
			Content *[]jsontext.Value `json:"content"`
		}
		if json.Unmarshal([]byte(body), &message) == nil && message.Content != nil {
			return parent, body
		}
	}
	return "", ""
}

package host

import (
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

// conversationTokensLocked also accepts older response usage until a provider
// has recorded its own context measurement. Response usage can sum many steps;
// the explicit measurement remains authoritative thereafter.
func (s *Session) conversationTokensLocked() map[string]int64 {
	tokens := map[string]int64{}
	explicit := map[string]bool{}
	provider := ""
	if s.selection != nil {
		provider = s.selection.Provider
	}
	for _, item := range s.items {
		switch data := item.Data.(type) {
		case sessionstore.HostRecord:
			switch data.Kind {
			case "configuration":
				if selection := sessionstore.SelectionFromConfiguration(data.Configuration); selection != nil {
					provider = selection.Provider
				}
			case sessionstore.HostRuntimeApplied:
				if data.Selection != nil {
					provider = data.Selection.Provider
				}
			case sessionstore.HostContext:
				if data.Context != nil && data.Context.Conversation != nil {
					r := data.Context.Conversation
					explicit[r.Provider] = true
					delete(tokens, r.Provider)
					if r.InputKnown {
						tokens[r.Provider] = r.InputTokens
					}
				}
			}
		case sessionstore.ModelResponse:
			if provider != "" && !explicit[provider] {
				if usage := llm.ReportedTokens(data.Response.Usage); usage.InputKnown {
					tokens[provider] = usage.Input
				}
			}
		}
	}
	return tokens
}

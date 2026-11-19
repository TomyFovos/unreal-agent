package host

import (
	"context"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func (s *Session) recordConversationBinding(ctx context.Context, binding llm.ConversationBinding) error {
	return (&serializedStore{Session: s}).AppendHostRecord(ctx, s.ID, sessionstore.HostRecord{
		Version: 1, Kind: sessionstore.HostContext,
		Context: &sessionstore.ContextRecord{Kind: sessionstore.ContextConversationBinding, Provider: binding.Provider, ConversationBinding: &binding},
	})
}

// conversationStateLocked also accepts older response usage until a provider
// has recorded its own context measurement. Response usage can sum many steps;
// the explicit measurement remains authoritative thereafter.
func (s *Session) conversationStateLocked() (map[string]int64, map[string]llm.ConversationBinding) {
	tokens := map[string]int64{}
	explicit := map[string]bool{}
	bindings := map[string]llm.ConversationBinding{}
	provider := ""
	if s.selection != nil {
		provider = s.selection.Provider
	}
	for _, item := range s.items {
		if item.Kind == sessionstore.ItemFork {
			clear(bindings)
		}
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
				if data.Context == nil {
					continue
				}
				if data.Context.Kind == sessionstore.ContextConversationBinding && data.Context.ConversationBinding != nil {
					binding := *data.Context.ConversationBinding
					bindings[binding.Provider] = binding
				}
				if data.Context.Conversation != nil {
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
	return tokens, bindings
}

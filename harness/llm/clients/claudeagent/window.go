package claudeagent

import (
	"context"
	"strings"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

const (
	disableAutoCompactEnv = "DISABLE_AUTO_COMPACT"
	disableCompactEnv     = "DISABLE_COMPACT"
)

// WindowPrompt renders the L2 copy and public L1 items as stream-json user
// content. Claude Code gets its usual system additions separately. The edge
// whitespace in user messages is part of the original text.
func WindowPrompt(items []llm.Item) string {
	return windowRenderer(items).Render(items)
}

func windowRenderer(items []llm.Item) llm.WindowPrompt {
	var scopes llm.ToolScopes
	scopes.Observe(items)
	return func(item llm.Item) (string, bool) {
		if scopes.Nested(item) {
			return "", false
		}
		return WindowPromptLine(item)
	}
}

func WindowPromptLine(item llm.Item) (string, bool) {
	switch v := item.Data.(type) {
	case llm.Message:
		switch v.Role {
		case llm.RoleUser:
			return v.Text, true
		case llm.RoleAssistant:
			return "[assistant] " + v.Text, true
		}
	case llm.ToolCall:
		return "[recorded tool call] " + v.Name + " " + v.Arguments, true
	case llm.ToolResult:
		var parts []string
		for _, output := range v.Output {
			if output.Kind == llm.ToolResultText {
				parts = append(parts, output.Value)
			}
		}
		return "[recorded tool result] " + strings.Join(parts, "\n"), true
	}
	return "", false
}

func (c *Client) conversationID(base string, opt llm.RequestOptions) string {
	if id := opt.ConversationBindings[c.providerID()].ID; id != "" {
		return id
	}
	return base
}

func (c *Client) planWindow(ctx context.Context, request llm.Request, opt llm.RequestOptions, base string) (string, promptPlan, error) {
	window := request.Window
	provider := c.providerID()
	previous := opt.ConversationBindings[provider]
	binding := llm.ConversationBinding{Provider: provider, Boundary: window.Boundary, ID: base}
	if previous.ID != "" && previous.Boundary == window.Boundary {
		binding = previous
	} else if window.Boundary != "" {
		// The same boundary selects the same ID even if a Host stops between
		// committing the window, selecting its conversation and starting it.
		binding.ID = sessionUUID("", base+"\x00"+window.Boundary)
	}
	if binding != previous && opt.RecordConversationBinding != nil {
		if err := opt.RecordConversationBinding(ctx, binding); err != nil {
			return "", promptPlan{}, &Error{Code: "record_conversation_binding", Message: err.Error()}
		}
	}
	render := windowRenderer(request.Input)
	full := render.Render(request.Input)
	delivered := window.DeliveredItems
	if delivered > 0 && delivered <= len(request.Input) && (binding.ID == previous.ID || binding.ID == base) {
		return binding.ID, promptPlan{resume: true, prompt: render.Render(request.Input[delivered:]), full: full}, nil
	}
	return binding.ID, promptPlan{prompt: full, full: full}, nil
}

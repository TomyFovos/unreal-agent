// Package claudeprompt projects Unreal history into the text sent to Claude Code.
// The adapter and pre-send token estimator share this projection.
package claudeprompt

import (
	"strings"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

// ItemPrefix marks history items this adapter produced. Claude Code's own
// session already holds them, so a later turn sends only what came after.
const ItemPrefix = "claude-code/"

const ReceiptPrefix = "Prior Unreal tool receipt"

type Prompt struct {
	Resume bool   // Claude Code's session holds this adapter's earlier turns
	New    string // what a resumed session needs
	Full   string // what a new session needs: all earlier context
}

// Plan splits the projected history into what Claude Code has already
// seen and what it has not: turns another model answered after Claude's last
// turn, and the new user input at the end.
func Plan(input []llm.Item) Prompt {
	var scopes llm.ToolScopes
	scopes.Observe(input)
	last := -1
	ownCalls := map[string]bool{}
	for i, item := range input {
		if scopes.Nested(item) {
			continue
		}
		if item.ClaudeResumeBoundary || strings.HasPrefix(item.ProviderID, ItemPrefix) {
			last = i
			if call, ok := item.Data.(llm.ToolCall); ok {
				ownCalls[call.CallID] = true
			}
		}
	}
	start := len(input)
	for start > last+1 {
		m, ok := input[start-1].Data.(llm.Message)
		if !ok || m.Role != llm.RoleUser || strings.HasPrefix(m.Text, ReceiptPrefix) {
			break
		}
		start--
	}
	var request []string
	for _, item := range input[start:] {
		if text := strings.TrimSpace(item.Data.(llm.Message).Text); text != "" {
			request = append(request, text)
		}
	}
	ask := strings.Join(request, "\n\n")
	if ask == "" {
		ask = "Unreal Agent resumed this turn after an interruption. Continue the task."
	}
	render := func(items []llm.Item) string {
		var lines []string
		for _, item := range items {
			if scopes.Nested(item) {
				continue
			}
			if line := transcriptLine(item, ownCalls); line != "" {
				lines = append(lines, line)
			}
		}
		if len(lines) == 0 {
			return ask
		}
		return "Conversation in Unreal Agent that you have not seen yet (it already happened; use it as context):\n\n" + strings.Join(lines, "\n\n") + "\n\nNew message:\n" + ask
	}
	plan := Prompt{Resume: last >= 0, Full: render(input[:start])}
	if plan.Resume {
		plan.New = render(input[last+1 : start])
	} else {
		plan.New = plan.Full
	}
	return plan
}

func transcriptLine(item llm.Item, ownCalls map[string]bool) string {
	switch v := item.Data.(type) {
	case llm.Message:
		if v.Role == llm.RoleSystem || strings.TrimSpace(v.Text) == "" {
			return ""
		}
		return "[" + string(v.Role) + "] " + v.Text
	case llm.ToolCall:
		return "[tool call] " + v.Name + " " + v.Arguments
	case llm.ToolResult:
		if ownCalls[v.CallID] {
			return ""
		}
		var parts []string
		for _, output := range v.Output {
			if output.Kind == llm.ToolResultText {
				parts = append(parts, output.Value)
			}
		}
		return "[tool result] " + strings.Join(parts, "\n")
	}
	return ""
}

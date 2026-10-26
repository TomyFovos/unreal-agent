package claudeagent

import (
	"fmt"
	"strings"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/permission"
)

// itemPrefix marks history items this adapter produced. Claude Code's own
// session already holds them, so a later turn sends only what came after.
const itemPrefix = "claude-code/"

const receiptPrefix = "Prior Unreal tool receipt"

type promptPlan struct {
	resume bool   // Claude Code's session holds this adapter's earlier turns
	prompt string // what a resumed session needs
	full   string // what a new session needs: all earlier context
}

// planPrompt splits the projected history into what Claude Code has already
// seen and what it has not: turns another model answered after Claude's last
// turn, and the new user input at the end.
func planPrompt(input []llm.Item) promptPlan {
	last := -1
	ownCalls := map[string]bool{}
	for i, item := range input {
		if strings.HasPrefix(item.ProviderID, itemPrefix) {
			last = i
			if call, ok := item.Data.(llm.ToolCall); ok {
				ownCalls[call.CallID] = true
			}
		}
	}
	start := len(input)
	for start > last+1 {
		m, ok := input[start-1].Data.(llm.Message)
		if !ok || m.Role != llm.RoleUser || strings.HasPrefix(m.Text, receiptPrefix) {
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
			if line := transcriptLine(item, ownCalls); line != "" {
				lines = append(lines, line)
			}
		}
		if len(lines) == 0 {
			return ask
		}
		return "Conversation in Unreal Agent that you have not seen yet (it already happened; use it as context):\n\n" + strings.Join(lines, "\n\n") + "\n\nNew message:\n" + ask
	}
	plan := promptPlan{resume: last >= 0, full: render(input[:start])}
	if plan.resume {
		plan.prompt = render(input[last+1 : start])
	} else {
		plan.prompt = plan.full
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

// appendedPrompt tells Claude how Unreal Agent treats its tool calls (U21,
// U22). It adds no rule of its own about denials: Claude Code's own guidance
// applies, and the user can ask again after a denial.
func appendedPrompt(mode string, subagents SubagentCLI) string {
	if mode == "" {
		mode = "set by the operator"
	}
	return fmt.Sprintf("You are running inside Unreal Agent. Unreal Agent records this conversation and decides tool approvals with its permission mode (now: %s).\n"+
		"When Claude Code refuses a shell command on its own (for example by its organization policy), Unreal Agent runs that exact command in its own shell after its own approval. Its result then arrives as additional context that starts with %q. Treat that as the command's real result and do not run the command again.", mode, ranPrefix) + subagentPrompt(subagents)
}

func modeLabel(controller *permission.Controller) string {
	if controller == nil {
		return ""
	}
	return controller.Snapshot().Mode.Label()
}

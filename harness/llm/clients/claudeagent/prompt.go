package claudeagent

import (
	"fmt"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/claudeprompt"
	"github.com/unreallabsai/unreal-agent/harness/permission"
)

const itemPrefix = claudeprompt.ItemPrefix
const receiptPrefix = claudeprompt.ReceiptPrefix

type promptPlan struct {
	resume bool
	prompt string
	full   string
}

func planPrompt(input []llm.Item) promptPlan {
	p := claudeprompt.Plan(input)
	return promptPlan{resume: p.Resume, prompt: p.New, full: p.Full}
}

// appendedPrompt tells Claude how Unreal Agent treats its tool calls (U21,
// U22). It adds no rule of its own about denials: Claude Code's own guidance
// applies, and the user can ask again after a denial.
func appendedPrompt(mode string, subagents SubagentCLI) string {
	if mode == "" {
		mode = "set by the operator"
	}
	return fmt.Sprintf("You are running inside Unreal Agent. Unreal Agent records this conversation and decides tool approvals with its permission mode (now: %s).\n"+
		"When you ask the user something, start that message with 【要確認】, including in ordinary foreground turns, and write choices as numbered prose. Ask once: Unreal Agent keeps an unanswered 【要確認】 on screen until the user replies, so do not repeat the question in later turns; a background turn that only notes progress ends with one short line and no 【要確認】. AskUserQuestion is unavailable.\n"+
		"When Claude Code declines a shell command itself, Unreal Agent handles it as an ordinary Bash request under its permission mode. Its result then arrives as additional context that starts with %q. Treat that as the command's real result and do not run the command again.", mode, ranPrefix) + subagentPrompt(subagents)
}

func modeLabel(controller *permission.Controller) string {
	if controller == nil {
		return ""
	}
	return controller.Snapshot().Mode.Label()
}

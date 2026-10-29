package claudeagent

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"
	"uuid"

	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

// The Read Only gate runs before Claude Code's own rules, which would otherwise
// approve their allowed commands by themselves.
const gateMatcher = "Bash|PowerShell|Edit|MultiEdit|Write|NotebookEdit"

// approver asks Unreal's permission mode about Claude Code's tool calls, one at
// a time: parallel prompts would make an open TUI prompt stale. cli is the
// Host's executable when Claude may drive subagents with it.
type approver struct {
	mu  sync.Mutex
	cli string
}

func (a *approver) decide(ctx context.Context, toolName string, input jsontext.Value, toolUseID string) (bool, string) {
	if toolName == "AskUserQuestion" {
		return false, "Unreal Agent cannot show question dialogs. Ask the user in your reply instead."
	}
	if toolName == "Bash" && subagentCommand(inputFields(input).Command, a.cli) {
		return true, ""
	}
	controller := permission.ControllerFromContext(ctx)
	if controller == nil {
		if err := policyCheck(ctx, toolName, input); err != nil {
			return false, "Unreal Agent's permission policy denied this tool call: " + err.Error()
		}
		return true, ""
	}
	name, scope := approvalSubject(toolName, input)
	readOnly := toolName == "Bash" && permission.ReadOnlyCommand(inputFields(input).Command) || toolName == "Read" && dropped(inputFields(input).FilePath)
	id := "claude:" + toolUseID
	if toolUseID == "" {
		id = "claude:" + uuid.New().String()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	// The controller always answers, including when ctx ends, so the permit is
	// never left unreleased.
	permit := <-controller.Request(ctx, permission.Request{ID: id, Tool: name, Scope: scope, Privileged: !readOnly, ReadOnlyCommand: readOnly})
	permit.Release()
	if permit.Denial == nil {
		return true, ""
	}
	return false, denialMessage(permit.Decision.State)
}

// gate answers the PreToolUse hook: only Read Only refuses here, except
// commands that only read; every other call goes on to Claude Code's rules
// and then to decide.
func (a *approver) gate(ctx context.Context, in hookInput) map[string]any {
	refuse := func(reason string) map[string]any {
		return map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "PreToolUse", "permissionDecision": "deny", "permissionDecisionReason": reason}}
	}
	if in.ToolName == "Bash" && subagentCommand(inputFields(in.ToolInput).Command, a.cli) {
		return map[string]any{} // children follow the mode themselves
	}
	if in.ToolName == "Bash" {
		if command, ok := currentSubagentCommand(inputFields(in.ToolInput).Command, a.cli); ok {
			var input map[string]any
			if json.Unmarshal(in.ToolInput, &input) == nil {
				input["command"] = command
				return map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "PreToolUse", "permissionDecision": "allow", "permissionDecisionReason": "Unreal Agent ran its current subagent command.", "updatedInput": input}}
			}
		}
	}
	if controller := permission.ControllerFromContext(ctx); controller != nil {
		readOnly := in.ToolName == "Bash" && permission.ReadOnlyCommand(inputFields(in.ToolInput).Command)
		if controller.Snapshot().Mode == permission.ReadOnly && !readOnly {
			return refuse("Unreal Agent is in Read Only mode, so " + in.ToolName + " calls are denied.")
		}
		return map[string]any{}
	}
	if err := policyCheck(ctx, in.ToolName, in.ToolInput); err != nil {
		return refuse("Unreal Agent's permission policy denied this tool call: " + err.Error())
	}
	return map[string]any{}
}

// policyCheck applies a fixed session policy, used where no human mode exists.
func policyCheck(ctx context.Context, toolName string, input jsontext.Value) error {
	policy := permission.FromContext(ctx)
	fields := inputFields(input)
	switch toolName {
	case "Bash", "PowerShell":
		return policy.CheckProcess()
	case "Edit", "MultiEdit", "Write", "NotebookEdit":
		return policy.CheckPath(first(fields.FilePath, fields.NotebookPath), true)
	case "Read":
		return policy.CheckPath(fields.FilePath, false)
	}
	return nil
}

type fields struct {
	FilePath     string `json:"file_path"`
	NotebookPath string `json:"notebook_path"`
	Command      string `json:"command"`
	URL          string `json:"url"`
	Path         string `json:"path"`
	Pattern      string `json:"pattern"`
}

func inputFields(input jsontext.Value) fields {
	var f fields
	_ = json.Unmarshal(input, &f)
	return f
}

// approvalSubject names the call with Unreal's own tool names, which the
// operator maximum lists, and describes it for the human in Scope.
func approvalSubject(toolName string, input jsontext.Value) (string, string) {
	f := inputFields(input)
	name, detail := tool.ClaudeCodeName, ""
	switch toolName {
	case "Edit", "MultiEdit", "NotebookEdit":
		name, detail = tool.EditName, first(f.FilePath, f.NotebookPath)
	case "Write":
		name, detail = tool.WriteName, f.FilePath
	case "Bash", "PowerShell":
		name, detail = tool.BashName, f.Command
	case "Read":
		name, detail = tool.ReadName, f.FilePath
	default:
		detail = first(f.URL, f.FilePath, f.Path, f.Pattern, f.Command)
		if detail == "" && len(input) > 0 {
			detail = string(input)
		}
	}
	scope := "Claude Code " + toolName
	if detail != "" {
		scope += ": " + detail
	}
	return name, clip(scope, 512)
}

func denialMessage(state string) string {
	switch state {
	case "read_only":
		return "Unreal Agent is in Read Only mode, so this tool call was denied."
	case "timeout":
		return "No one approved this tool call in Unreal Agent in time, so it was denied."
	case "canceled", "revoked":
		return "Unreal Agent canceled the approval for this tool call."
	}
	return "Unreal Agent denied this tool call."
}

// dropped reports a file the user sent from Windows into ~/drop: reading it
// is reading what the user handed over, in every mode.
func dropped(path string) bool {
	home, err := os.UserHomeDir()
	if err != nil || path == "" || !filepath.IsAbs(path) {
		return false
	}
	rel, err := filepath.Rel(filepath.Join(home, "drop"), filepath.Clean(path))
	return err == nil && rel != "." && !strings.HasPrefix(rel, "..")
}

func first(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// clip keeps one line of at most limit bytes, cut on a rune boundary.
func clip(text string, limit int) string {
	text = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(text, "�"))
	if len(text) <= limit {
		return text
	}
	cut := limit - len("…")
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + "…"
}

package claudeagent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"github.com/unreallabsai/unreal-agent/harness/tool/observed"
)

// TestLiveRefusedCommandReachesUnreal runs the signed-in claude CLI (skipped
// unless UNREAL_LIVE_CLAUDE=1) and asks for a command the organization policy
// refuses. Unreal must receive it as its own Bash call (U22). The rendezvous
// here answers with a canned result: nothing runs the refused command.
func TestLiveRefusedCommandReachesUnreal(t *testing.T) {
	if os.Getenv("UNREAL_LIVE_CLAUDE") != "1" {
		t.Skip("set UNREAL_LIVE_CLAUDE=1 to use the signed-in Claude Code subscription")
	}
	binary := os.Getenv("UNREAL_LIVE_CLAUDE_BINARY")
	if binary == "" {
		binary = filepath.Join(os.Getenv("HOME"), ".local", "bin", "claude")
	}
	results := observed.NewResults()
	c, err := NewClient(Config{Binary: binary, Directory: t.TempDir(), Results: results})
	if err != nil {
		t.Fatal(err)
	}
	var calls []llm.ToolCall
	tools := func(ctx context.Context, response llm.Response) ([]llm.ToolOutcome, error) {
		var outcomes []llm.ToolOutcome
		for _, item := range response.Output {
			if call, ok := item.Data.(llm.ToolCall); ok {
				calls = append(calls, call)
				text := "recorded"
				if call.Name == tool.BashName {
					text = "exit code 128\nfatal: not a git repository (canned by the test; nothing ran)"
				}
				outcomes = append(outcomes, llm.ToolOutcome{Result: llm.ToolResult{CallID: call.CallID, Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: text}}}})
			}
		}
		return outcomes, nil
	}
	command := os.Getenv("UNREAL_LIVE_CLAUDE_REFUSED")
	if command == "" {
		command = "git push"
	}
	model := os.Getenv("UNREAL_LIVE_CLAUDE_MODEL")
	if model == "" {
		model = "haiku"
	}
	response, err := c.Respond(t.Context(), llm.Request{Model: llm.Model{ID: model}, Input: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "Run this exact Bash command once: " + command}}}}, llm.RequestOptions{Tools: tools})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, call := range calls {
		names = append(names, call.Name+" "+call.Arguments)
	}
	t.Logf("recorded calls: %s", strings.Join(names, " | "))
	t.Logf("final: %+v", response.Output)
	handed := false
	for _, call := range calls {
		handed = handed || call.Name == tool.BashName && strings.Contains(call.Arguments, command)
	}
	if !handed {
		t.Fatal("the refused command was not handed to Unreal's Bash")
	}
}

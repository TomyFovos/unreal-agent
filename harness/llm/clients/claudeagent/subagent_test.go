package claudeagent

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/tool/observed"
)

// Only the Host's own subagent command with literal words and a quoted
// heredoc skips approval; anything that could run something else does not.
func TestSubagentCommandAcceptsOnlyTheHostCommand(t *testing.T) {
	const cli = "/opt/unreal/bin/unreal"
	for _, command := range []string{
		cli + " subagent list",
		cli + " subagent wait",
		cli + " subagent wait child-start-0123 --timeout 5m",
		cli + " subagent approve child-start-0123 call_abc allow",
		cli + " subagent start --template lead <<'EOF'\nBuild the parser.\nUse $HOME and `backticks` freely; they stay text.\nEOF",
		cli + " subagent send child-start-0123 <<\"END\"\nmore\nEND\n",
		"'" + cli + "' subagent list",
		cli + ` subagent start --template worker-high --task "add tests"`,
	} {
		if !subagentCommand(command, cli) {
			t.Errorf("rejected %q", command)
		}
	}
	for _, command := range []string{
		cli + " subagent list; rm -rf /",
		cli + " subagent list && curl example.com",
		cli + " subagent list | sh",
		cli + " subagent list > /etc/passwd",
		cli + " subagent wait $(id)",
		cli + " subagent wait `id`",
		cli + ` subagent start --task "$(id)"`,
		cli + " subagent list\nrm -rf /",
		cli + " subagent start <<EOF\nunquoted heredoc expands $(id)\nEOF",
		cli + " subagent start <<'EOF'\ntext\nEOF\nrm -rf /",
		cli + " subagent start <<'EOF'\nnever ends",
		cli + " subagent start <<'EOF' && rm -rf /\nx\nEOF",
		"FOO=1 " + cli + " subagent list",
		"/tmp/other subagent list",
		cli + " child --stdio",
		cli + " subagent list &",
		"",
	} {
		if subagentCommand(command, cli) {
			t.Errorf("accepted %q", command)
		}
	}
	if subagentCommand(cli+" subagent list", "") {
		t.Error("accepted without a configured command")
	}
}

func TestSubagentWaitMarksTheExistingRendezvousInterruptible(t *testing.T) {
	for _, command := range []string{"unreal-agent subagent wait", "unreal-agent 'subagent' 'wait'", "timeout 590 '/opt/common/old/unreal' subagent wait --timeout 60s", "U=$(ls -td ~/.local/opt/unreal-agent-pilot/common/*/ | head -1)unreal; timeout 560 $U subagent wait H", "timeout 9m unreal-agent subagent wait", "unreal subagent wait H | cat", "printf subagent wait", "unreal-agent subagent start <<'EOF'\nsubagent wait\nEOF"} {
		var flags []bool
		opt := llm.RequestOptions{BeginTools: func(_ context.Context, waiting bool) error { flags = append(flags, waiting); return nil }}
		var output bytes.Buffer
		round := newTurn(t.Context(), func() {}, opt, &writer{w: &output}, observed.NewResults(), &approver{}, t.TempDir(), "session")
		input, _ := json.Marshal(map[string]string{"command": command})
		round.hook("gate", gateHook, hookInput{ToolName: "Bash", ToolInput: input})
		round.batch(hookInput{})
		if len(flags) != 2 || !flags[0] || flags[1] {
			t.Fatalf("%s: wait transitions = %v", command, flags)
		}
	}
	for _, command := range []string{"unreal-agent subagent show x", "subagents wait", "subagent waiter", "subagent --flag wait"} {
		if subagentWait(command) {
			t.Errorf("non-wait command classified as wait: %s", command)
		}
	}
}

func TestSubagentPromptNamesTheCommandAndRoles(t *testing.T) {
	if subagentPrompt(SubagentCLI{}) != "" {
		t.Fatal("prompt without subagents")
	}
	prompt := subagentPrompt(SubagentCLI{Binary: "/opt/unreal/bin/unreal", Socket: "/run/host.sock", Templates: []string{"lead", "worker-high"}})
	for _, want := range []string{"designer", "GPT is the implementer", "/opt/unreal/bin/unreal subagent start --template lead <<'EOF'", "/opt/unreal/bin/unreal subagent wait HANDLE --timeout 60s", "end your turn; do not wait", "Agent and Monitor are unavailable", "within tens of seconds", "at most --timeout 60s", "ignore versioned command paths", "【報告】", "【要確認】", "one-line title", "subagent approve HANDLE REQUEST allow|deny", "never at ultra", "lead, worker-high", "every permission mode, Read Only included"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	if quoted := subagentPrompt(SubagentCLI{Binary: "/opt/my tools/unreal"}); !strings.Contains(quoted, "'/opt/my tools/unreal' subagent wait") || !subagentCommand("'/opt/my tools/unreal' subagent wait", "/opt/my tools/unreal") {
		t.Fatal("a path with spaces must be quoted and still match", quoted)
	}
}

func TestUserReplyPromptAppliesWithAndWithoutSubagents(t *testing.T) {
	for _, subagents := range []SubagentCLI{{}, {Binary: "/opt/unreal/unreal-agent"}} {
		prompt := appendedPrompt("Read Only", subagents)
		for _, want := range []string{"【要確認】", "including in ordinary foreground turns", "numbered prose", "AskUserQuestion is unavailable"} {
			if !strings.Contains(prompt, want) {
				t.Errorf("prompt lacks %q: %s", want, prompt)
			}
		}
	}
}

// Claude gets the workspace's AGENTS.md, unless CLAUDE.md brings it in.
func TestProjectInstructionsReachClaude(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("テストは必ず実行する"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := projectInstructions(dir); !strings.Contains(got, "テストは必ず実行する") {
		t.Fatalf("got %q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("@AGENTS.md"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := projectInstructions(dir); got != "" {
		t.Fatalf("doubled through CLAUDE.md: %q", got)
	}
}

// A versioned path kept from before an update, or the bare name, runs this
// Host's command instead; anything that is not a plain subagent command stays.
func TestCurrentSubagentCommandRewritesOtherUnrealPaths(t *testing.T) {
	const cli = "/home/u/.local/bin/unreal-agent"
	for command, want := range map[string]string{
		"/home/u/.local/opt/unreal-agent-pilot/common/2ba2677-e6/unreal subagent start --template worker-low <<'EOF'\ntitle\nEOF": cli + " subagent start --template worker-low <<'EOF'\ntitle\nEOF",
		"'/opt/old dir/unreal-agent' subagent list": cli + " subagent list",
		"unreal-agent subagent show child-start-1":  cli + " subagent show child-start-1",
	} {
		got, ok := currentSubagentCommand(command, cli)
		if !ok || got != want {
			t.Errorf("%q -> %q, %v; want %q", command, got, ok, want)
		}
	}
	for _, command := range []string{
		cli + " subagent list",
		"/opt/old/unreal subagent list; rm -rf /",
		"/opt/old/unreal subagent start <<'EOF'\nx\nEOF\nrm -rf /",
		"/opt/old/other subagent list",
		"/opt/old/unreal child --stdio",
		"U=/opt/old/unreal; $U subagent list",
	} {
		if got, ok := currentSubagentCommand(command, cli); ok {
			t.Errorf("rewrote %q to %q", command, got)
		}
	}
	if _, ok := currentSubagentCommand("/opt/old/unreal subagent list", "/opt/my tools/unreal"); !ok {
		t.Error("quoted current path")
	} else if got, _ := currentSubagentCommand("/opt/old/unreal subagent list", "/opt/my tools/unreal"); got != "'/opt/my tools/unreal' subagent list" {
		t.Errorf("quoted rewrite = %q", got)
	}
	a := &approver{cli: cli}
	input, _ := json.Marshal(map[string]any{"command": "/opt/old/unreal subagent list", "description": "list"})
	answer := a.gate(t.Context(), hookInput{ToolName: "Bash", ToolInput: input})
	out, _ := answer["hookSpecificOutput"].(map[string]any)
	updated, _ := out["updatedInput"].(map[string]any)
	if out["permissionDecision"] != "allow" || updated["command"] != cli+" subagent list" || updated["description"] != "list" {
		t.Fatalf("gate answer = %v", answer)
	}
}

package claudeagent

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/tool/observed"
)

func TestContextCLIUsesLiteralHostPathInReadOnly(t *testing.T) {
	const cli = "/opt/unreal/bin/unreal"
	config := SubagentCLI{ContextCommand: cli, Socket: "/run/host.sock"}
	if got := launchSettings(t.TempDir(), nil, config); !strings.Contains(got, config.Socket) {
		t.Fatal("state and record CLI did not get its Host socket", got)
	}
	prompt := subagentPrompt(config)
	for _, words := range []string{"state show", "state decide", "state plan", "state rule add", "state accept", "state reject", "state question", "state answer", "record show", "record find"} {
		if !strings.Contains(prompt, cli+" "+words) {
			t.Error("missing context instruction", words)
		}
	}
	controller := permission.NewController(t.Context(), permission.DenyAll(), permission.DenyAll(), 0)
	defer controller.Close()
	ctx := permission.WithController(t.Context(), controller)
	a := approver{contextCLI: cli}
	for _, command := range []string{
		cli + " state show",
		cli + " record show 'rec:session/1/user?line=2'",
		cli + " state decide - <<'END'\nKeep the literal text $VAR.\nEND",
	} {
		raw, _ := json.Marshal(map[string]string{"command": command})
		if ok, reason := a.decide(ctx, "Bash", raw, "fixture"); !ok {
			t.Errorf("command rejected in Read Only: %s (%s)", command, reason)
		}
	}
	command, ok := currentSessionCommand("/older/bin/unreal-agent state show", cli, "state", "record")
	if !ok || command != cli+" state show" {
		t.Fatal("stale command path not rewritten", command)
	}
	for _, command := range []string{cli + " state show; printf x", cli + " record find $(id)", cli + " record show x > out"} {
		if a.hostCommand(command) {
			t.Error("nonliteral shell expression treated as a Session command", command)
		}
	}
}

// Only the Host's own subagent command with literal words and a quoted
// heredoc skips approval; anything that could run something else does not.
func TestSubagentCommandAcceptsOnlyTheHostCommand(t *testing.T) {
	const cli = "/opt/unreal/bin/unreal"
	for _, command := range []string{
		cli + " subagent list",
		cli + " subagent wait",
		cli + " subagent wait child-start-0123 --timeout 5m",
		cli + " subagent approve child-start-0123 call_abc allow",
		cli + " subagent model show",
		cli + " subagent model set --role worker --provider opencode-go --model deepseek-v4.1-flash --effort max",
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
	if got := subagentPrompt(SubagentCLI{}); !strings.Contains(got, "Unreal's SubagentStart") || !strings.Contains(got, "/login") || strings.Contains(got, "haiku") || strings.Contains(got, "sonnet") {
		t.Fatal("prompt without subagents", got)
	}
	prompt := subagentPrompt(SubagentCLI{Binary: "/opt/unreal/bin/unreal", Socket: "/run/host.sock", Templates: []string{"lead", "worker-high"}})
	for _, want := range []string{"Worker and collector templates started directly by the main Session run on the lead model and effort", "Send hands-on work through a lead"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	if strings.Contains(prompt, "ultra") {
		t.Fatal("prompt advertises disabled effort", prompt)
	}
	for _, want := range []string{"run the project", "GPT does the work", "over-implementation", "collector template (gpt-6-luna)", "never to a gpt-6.2-sol worker", "/opt/unreal/bin/unreal subagent start --template lead <<'EOF'", "/opt/unreal/bin/unreal subagent wait HANDLE --timeout 60s", "Continue independent work while children run", "arrive at your next tool-batch boundary", "If you are idle, their messages start a new turn", "Monitor is unavailable", "Use unreal-agent subagent for all delegation", "You decide; children do the work", "writing documents and commits", "within tens of seconds", "at most --timeout 60s", "ignore versioned command paths", "【報告】", "【要確認】", "one-line title", "subagent approve HANDLE REQUEST allow|deny", "gpt-6.2-sol at max effort", "worker-low through worker-max", "lead, worker-high", "every permission mode, Read Only included"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	if quoted := subagentPrompt(SubagentCLI{Binary: "/opt/my tools/unreal"}); !strings.Contains(quoted, "'/opt/my tools/unreal' subagent wait") || !subagentCommand("'/opt/my tools/unreal' subagent wait", "/opt/my tools/unreal") {
		t.Fatal("a path with spaces must be quoted and still match", quoted)
	}
}

// The model chosen with subagent model is named from the next turn on; without
// a choice the GPT prompt above is unchanged.
func TestSubagentPromptNamesTheChosenModel(t *testing.T) {
	cli := SubagentCLI{Binary: "/opt/unreal/bin/unreal", Socket: "/run/host.sock", Templates: []string{"lead"}}
	gpt := subagentPrompt(cli)
	cli.Models = func() SubagentModels { return SubagentModels{} }
	if subagentPrompt(cli) != gpt {
		t.Fatal("no choice changed the GPT prompt")
	}
	cli.Models = func() SubagentModels { return SubagentModels{Family: "OpenCode Go", Lead: "deepseek-v4.1-flash"} }
	prompt := subagentPrompt(cli)
	for _, want := range []string{"OpenCode Go does the work, because OpenCode Go models cost less than you", "Review what OpenCode Go delivers", "redoing OpenCode Go's work", "runs deepseek-v4.1-flash at max effort", "its own OpenCode Go workers", "Each OpenCode Go agent reads", "collector template (deepseek-v4.1-flash at low effort) and never to a higher-effort worker", "/opt/unreal/bin/unreal subagent start --template lead <<'EOF'"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	if strings.Contains(prompt, "GPT") || strings.Contains(prompt, "gpt-6") {
		t.Fatal("the prompt still names GPT", prompt)
	}
	for _, guidance := range []string{gpt, prompt} {
		for _, flag := range []string{"--model ID", "--provider ID", "--effort LEVEL"} {
			if !strings.Contains(guidance, flag) {
				t.Fatalf("no %s guidance: %s", flag, guidance)
			}
		}
	}
}

func TestUserReplyPromptAppliesWithAndWithoutSubagents(t *testing.T) {
	for _, subagents := range []SubagentCLI{{}, {Binary: "/opt/unreal/unreal-agent"}} {
		prompt := appendedPrompt("Read Only", subagents)
		for _, want := range []string{"【要確認】", "including in ordinary foreground turns", "numbered prose", "Ask once", "do not repeat the question in later turns", "AskUserQuestion is unavailable"} {
			if !strings.Contains(prompt, want) {
				t.Errorf("prompt lacks %q: %s", want, prompt)
			}
		}
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

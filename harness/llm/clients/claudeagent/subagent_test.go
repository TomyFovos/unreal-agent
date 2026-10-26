package claudeagent

import (
	"strings"
	"testing"
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

func TestSubagentPromptNamesTheCommandAndRoles(t *testing.T) {
	if subagentPrompt(SubagentCLI{}) != "" {
		t.Fatal("prompt without subagents")
	}
	prompt := subagentPrompt(SubagentCLI{Binary: "/opt/unreal/bin/unreal", Socket: "/run/host.sock", Templates: []string{"lead", "worker-high"}})
	for _, want := range []string{"designer", "GPT is the implementer", "/opt/unreal/bin/unreal subagent start --template lead <<'EOF'", "/opt/unreal/bin/unreal subagent wait HANDLE", "600000 ms timeout", "subagent approve HANDLE REQUEST allow|deny", "never at ultra", "lead, worker-high", "every permission mode, Read Only included"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	if quoted := subagentPrompt(SubagentCLI{Binary: "/opt/my tools/unreal"}); !strings.Contains(quoted, "'/opt/my tools/unreal' subagent wait") || !subagentCommand("'/opt/my tools/unreal' subagent wait", "/opt/my tools/unreal") {
		t.Fatal("a path with spaces must be quoted and still match", quoted)
	}
}

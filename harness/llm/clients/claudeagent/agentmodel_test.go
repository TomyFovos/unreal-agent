package claudeagent

import (
	"slices"
	"strings"
	"testing"
)

// Without a choice, the launch environment and the delegation prompt are
// upstream's, byte for byte.
func TestAgentModelLeavesTheEnvironmentAndPromptAsTheyWere(t *testing.T) {
	base := []string{"A=1", "HOME=/home"}
	for _, s := range []SubagentCLI{{}, {AgentModel: func() string { return "" }}} {
		if got := agentModelEnv(slices.Clone(base), s); !slices.Equal(got, base) {
			t.Fatal("the environment changed without a model", got)
		}
		if got := claudeDelegationPromptFor(s); got != claudeDelegationPrompt {
			t.Fatal("the prompt changed without a model", got)
		}
	}
}

// A chosen Claude model names the Agent's model in the environment and forces
// it on every sub-agent, built-in Explore and Plan included, and it names the
// model in the delegation prompt.
func TestAgentModelNamesTheChosenModel(t *testing.T) {
	s := SubagentCLI{AgentModel: func() string { return "opus" }}
	if env, want := agentModelEnv([]string{"A=1"}, s), []string{"A=1", claudeSubagentModelEnv + "=opus", claudeSubagentModelForceEnv + "=1"}; !slices.Equal(env, want) {
		t.Fatal("the Agent model environment", env)
	}
	prompt := claudeDelegationPromptFor(s)
	if !strings.Contains(prompt, `to the Agent tool, which runs them on opus, and review`) || strings.Contains(prompt, "haiku") || strings.Contains(prompt, "sonnet") {
		t.Fatal("the prompt does not name the model", prompt)
	}
	if want := strings.Replace(claudeDelegationPrompt, claudeAgentModelRule, "to the Agent tool, which runs them on opus, and review", 1); prompt != want {
		t.Fatal("more of the prompt changed than the model")
	}
}

// While Unreal's own children run, the environment stays as it was.
func TestAgentModelIsNotSetWithChildren(t *testing.T) {
	s := SubagentCLI{Binary: "/opt/unreal/bin/unreal", AgentModel: func() string { return "opus" }}
	if env := agentModelEnv([]string{"A=1"}, s); !slices.Equal(env, []string{"A=1"}) {
		t.Fatal("the Agent model environment leaked with children", env)
	}
}

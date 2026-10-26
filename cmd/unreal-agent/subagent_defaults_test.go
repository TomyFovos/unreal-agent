package main

import (
	"slices"
	"testing"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

// The user's structure: the lead runs gpt-6.1-sol at ultra and starts
// workers, which never run at ultra and start nobody.
func TestDefaultSubagentsLeadAtUltraWorkersBelow(t *testing.T) {
	workspace := t.TempDir()
	cfg := codexServeConfiguration(workspace, "https://codex.invalid")
	cfg.Runtime.Provider.Provider = "claude"
	cfg.Providers = map[string]agentrunner.ProviderRuntime{"openai-codex": {Provider: codexServeConfiguration(workspace, "https://codex.invalid").Runtime.Provider}}
	cfg.Permissions = permission.Config{Tools: []string{"read", "grep", "glob"}, ReadRoots: []string{workspace}}
	maximum := permission.Config{Tools: append(tool.StaticNames(), tool.ClaudeCodeName), FilesystemUnrestricted: true, NetworkUnrestricted: true, ProcessMode: permission.ProcessUnrestricted}
	cfg.PermissionModes = &permission.ModesConfig{Enabled: true, Maximum: &maximum}
	templates := defaultSubagents(cfg)
	lead, ok := templates["lead"]
	if !ok || len(templates) != 1+len(workerEfforts) {
		t.Fatal("templates", len(templates))
	}
	if lead.Runtime.Provider.Provider != "openai-codex" || lead.Runtime.Provider.Model.ID != "gpt-6.1-sol" || lead.Runtime.ReasoningEffort != llm.ReasoningEffortUltra || !lead.Pinned || len(lead.Workers) != len(workerEfforts) {
		t.Fatalf("lead %+v", lead.Runtime)
	}
	for _, name := range childToolNames {
		if !slices.Contains(lead.Permissions.Tools, name) {
			t.Fatal("lead lacks", name)
		}
	}
	for name, worker := range lead.Workers {
		if worker.Runtime.ReasoningEffort == llm.ReasoningEffortUltra || len(worker.Workers) != 0 || slices.Contains(worker.Permissions.Tools, "SubagentStart") || !slices.Contains(worker.Permissions.Tools, "Finish") || !slices.Contains(worker.Permissions.Tools, "Bash") {
			t.Fatalf("worker %s: %+v", name, worker)
		}
		if templates[name].Runtime.ReasoningEffort != worker.Runtime.ReasoningEffort {
			t.Fatal("Claude's direct worker differs from the lead's", name)
		}
	}
	for _, template := range templates {
		if slices.Contains(template.Permissions.Tools, tool.ClaudeCodeName) || template.Baseline == nil || !slices.Equal(template.Baseline.Tools, cfg.Permissions.Tools) {
			t.Fatal("template policy", template.Permissions.Tools, template.Baseline)
		}
	}
	if extended := withChildTools(maximum); !slices.Contains(extended.Tools, "SubagentApprove") || slices.Contains(maximum.Tools, "SubagentApprove") {
		t.Fatal("withChildTools must extend a copy")
	}
	without := cfg
	without.Providers = nil
	if defaultSubagents(without) != nil {
		t.Fatal("subagents without Codex")
	}
	without = cfg
	without.PermissionModes = nil
	if defaultSubagents(without) != nil {
		t.Fatal("subagents without permission modes")
	}
}

package main

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/subagent"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

func TestLegacyUltraRuntimeConfigurationAndChildTemplatesLoad(t *testing.T) {
	cfg := codexServeConfiguration(t.TempDir(), "https://codex.invalid")
	cfg.Runtime.ReasoningEffort = llm.ReasoningEffortUltra
	worker := childTemplate{Runtime: cfg.Runtime, Pinned: true}
	cfg.Subagents = map[string]childTemplate{"legacy-lead": {Runtime: cfg.Runtime, Pinned: true, Workers: map[string]childTemplate{"legacy-worker": worker}}}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "runtime.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := readServeConfiguration(path)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := runtimeProviders(loaded.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	config := agentrunner.RuntimeConfig{Identity: loaded.Runtime, SessionDirectory: t.TempDir(), Providers: registry,
		Credentials: credential.ResolverFunc(func(context.Context, credential.Reference) (credential.Material, error) {
			return credential.Material{}, nil
		})}
	_, templates, err := withParentSubagents(config, loaded.Subagents, permission.Unrestricted(), "", "")
	if err != nil {
		t.Fatal("legacy templates rejected", err)
	}
	lead := templates["legacy-lead"]
	if lead.Default == nil || lead.Default.Effort.Normalize() != llm.ReasoningEffortMax || len(lead.Subagents) != 1 {
		t.Fatal("legacy lead", lead.Default, lead.Subagents)
	}
	// Older parents recorded pinned defaults literally. Regenerating their
	// creation identity and replaying plans must preserve those same bytes.
	legacyDefault, err := json.Marshal(lead.Default)
	if err != nil || !bytes.Contains(legacyDefault, []byte(`"effort":"ultra"`)) {
		t.Fatal("legacy pinned default changed creation identity", string(legacyDefault), err)
	}
	creation, err := withSubagentIdentity(jsontext.Value(`{"Version":1}`), templates)
	if err != nil {
		t.Fatal(err)
	}
	var restored struct {
		Subagents map[string]subagent.Template
	}
	if err := json.Unmarshal(creation, &restored); err != nil {
		t.Fatal(err)
	}
	replayed, err := withSubagentIdentity(jsontext.Value(`{"Version":1}`), restored.Subagents)
	if err != nil || creation.Canonicalize() != nil || replayed.Canonicalize() != nil || !bytes.Equal(creation, replayed) {
		t.Fatal("template replay changed parent creation identity", err)
	}
	for _, raw := range []jsontext.Value{lead.Runtime, lead.Subagents["legacy-worker"].Runtime} {
		selection := sessionstore.SelectionFromConfiguration(raw)
		if selection == nil || selection.Effort != llm.ReasoningEffortMax {
			t.Fatal("legacy template selection", selection)
		}
	}
	if err := subagent.ValidateWorkers(lead.Subagents); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, data) {
		t.Fatal("runtime.json was rewritten", err)
	}
}

// The user's structure: the lead runs gpt-6.1-sol at max and starts
// workers at low through max, which start nobody.
func TestDefaultSubagentsLeadAtMaxWorkersUnchanged(t *testing.T) {
	workspace := t.TempDir()
	cfg := codexServeConfiguration(workspace, "https://codex.invalid")
	cfg.Runtime.Provider.Provider = "claude"
	cfg.Providers = map[string]agentrunner.ProviderRuntime{"openai-codex": {Provider: codexServeConfiguration(workspace, "https://codex.invalid").Runtime.Provider}}
	cfg.Permissions = permission.Config{Tools: []string{"read", "grep", "glob"}, ReadRoots: []string{workspace}}
	maximum := permission.Config{Tools: append(tool.StaticNames(), tool.ClaudeCodeName), FilesystemUnrestricted: true, NetworkUnrestricted: true, ProcessMode: permission.ProcessUnrestricted}
	cfg.PermissionModes = &permission.ModesConfig{Enabled: true, Maximum: &maximum}
	templates := defaultSubagents(cfg)
	lead, ok := templates["lead"]
	if !ok || len(templates) != 2+len(workerEfforts) {
		t.Fatal("templates", len(templates))
	}
	if lead.Runtime.Provider.Provider != "openai-codex" || lead.Runtime.Provider.Model.ID != "gpt-6.1-sol" || lead.Runtime.ReasoningEffort != llm.ReasoningEffortMax || !lead.Pinned || len(lead.Workers) != 1+len(workerEfforts) {
		t.Fatalf("lead %+v", lead.Runtime)
	}
	// Low-difficulty tasks go to the collector on gpt-6-luna; it cannot start
	// children and is offered to Claude and the lead alike.
	if c, ok := templates["collector"]; !ok || c.Runtime.Provider.Model.ID != "gpt-6-luna" || c.Runtime.ReasoningEffort != llm.ReasoningEffortMedium || len(c.Workers) != 0 || slices.Contains(c.Permissions.Tools, "SubagentStart") || lead.Workers["collector"].Runtime.Provider.Model.ID != "gpt-6-luna" {
		t.Fatalf("collector %+v", c.Runtime)
	}
	for _, want := range []string{"collector template (gpt-6-luna)", "never to a gpt-6.1-sol worker", "nothing more"} {
		if !strings.Contains(lead.Runtime.SystemPrompt, want) {
			t.Fatalf("lead prompt lacks %q", want)
		}
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
		if strings.Contains(template.Runtime.SystemPrompt, "ultra") {
			t.Fatal("template prompt advertises disabled effort")
		}
		if slices.Contains(template.Permissions.Tools, tool.ClaudeCodeName) || template.Baseline == nil || !slices.Equal(template.Baseline.Tools, cfg.Permissions.Tools) {
			t.Fatal("template policy", template.Permissions.Tools, template.Baseline)
		}
		assertProgressGuidance(t, template.Runtime.SystemPrompt)
		for _, worker := range template.Workers {
			assertProgressGuidance(t, worker.Runtime.SystemPrompt)
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

func assertProgressGuidance(t *testing.T, prompt string) {
	t.Helper()
	for _, rule := range []string{"Send progress updates with SendParent progress: true", "without waking it", "For questions or requests needing a decision, omit progress (false by default) to wake the parent"} {
		if !strings.Contains(prompt, rule) {
			t.Fatalf("child prompt lacks %q: %s", rule, prompt)
		}
	}
}

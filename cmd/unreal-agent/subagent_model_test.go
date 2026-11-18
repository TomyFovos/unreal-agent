package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudeagent"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/opencodego"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/subagent"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

func TestSubagentRoleModelsSaveLoadAndLegacy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subagent-model.json")
	legacy := subagentModel{Version: 1, Provider: opencodego.ProviderID, Model: "deepseek-v4.1-flash"}
	if err := writeSubagentModel(path, legacy); err != nil {
		t.Fatal(err)
	}
	loaded, err := readSubagentModel(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range subagentRoleGroups {
		if got := loaded.choice(role); got != (subagentModelChoice{Provider: legacy.Provider, Model: legacy.Model}) {
			t.Fatalf("legacy %s: %+v", role, got)
		}
	}
	loaded.setRole("lead", subagentModelChoice{Provider: "openai-codex", Model: leadModel})
	loaded.setRole("worker", subagentModelChoice{Provider: opencodego.ProviderID, Model: legacy.Model, Effort: "max"})
	loaded.setRole("collector", subagentModelChoice{Provider: claudeagent.ProviderID, Model: "sonnet"})
	if err := writeSubagentModel(path, loaded); err != nil {
		t.Fatal(err)
	}
	restored, err := readSubagentModel(path)
	if err != nil || !reflect.DeepEqual(restored, loaded) {
		t.Fatalf("round trip: %+v, %v", restored, err)
	}
	for _, name := range []string{"worker-low", "worker-medium", "worker-high", "worker-xhigh", "worker-max"} {
		if got := restored.choice(name); got.Provider != opencodego.ProviderID || got.Effort != "max" {
			t.Fatalf("%s: %+v", name, got)
		}
	}
	if subagentAgentModel(path)() != "" {
		t.Fatal("a mixed choice forced every Claude Agent to one model")
	}
	for _, invalid := range []string{
		`{"Version":2,"Roles":{"worker":{"Model":"x"}}}`,
		`{"Version":2,"Roles":{"worker":{"Provider":"opencode-go"}}}`,
		`{"Version":2,"Roles":{"worker":{"Provider":"opencode-go","Model":"flash","Effort":"ultra"}}}`,
		`{"Version":2,"Roles":{"lead":{"Provider":"openai-codex","Effort":"ultra"}}}`,
		`{"Version":2,"Roles":{"typo":{"Provider":"openai-codex"}}}`,
	} {
		if err := os.WriteFile(path, []byte(invalid), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readSubagentModel(path); err == nil {
			t.Fatalf("accepted %s", invalid)
		}
	}
}

func TestSubagentOverrideRefusalUsesRole(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subagent-model.json")
	m := subagentModel{Version: 1, Provider: "openai-codex"}
	m.setRole("worker", subagentModelChoice{Provider: opencodego.ProviderID, Model: "deepseek-v4.1-flash", Effort: "max"})
	m.setRole("collector", subagentModelChoice{Provider: claudeagent.ProviderID, Model: "sonnet"})
	if err := writeSubagentModel(path, m); err != nil {
		t.Fatal(err)
	}
	template := subagent.Template{Default: &subagent.RuntimeRequest{Provider: "openai-codex", Model: leadModel}}
	for _, name := range []string{"lead", "worker-low", "worker-max", "collector"} {
		err := subagentOverrideRefusal(path, template, &subagent.RuntimeRequest{Effort: "high"}, false, name)
		if name == "lead" {
			if err != nil {
				t.Fatal(err)
			}
			continue
		}
		var refusal *subagent.RuntimeError
		if !errors.As(err, &refusal) || refusal.Code != "override_unavailable" {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestBoundWorkerRuntimeKeepsProviderBoundaries(t *testing.T) {
	for _, id := range []string{"openai-codex", opencodego.ProviderID, claudeagent.ProviderID} {
		cfg := codexServeConfiguration(t.TempDir(), "https://codex.invalid")
		cfg.Runtime.Provider.Provider = id
		raw, err := json.Marshal(cfg.Runtime)
		if err != nil {
			t.Fatal(err)
		}
		_, err = boundWorkerRuntime(t.Context(), nil, "collector", subagent.Template{Runtime: raw}, nil)
		if id == claudeagent.ProviderID {
			var refusal *subagent.RuntimeError
			if !errors.As(err, &refusal) || refusal.Code != "provider_unavailable" || refusal.Hint != claudeChildrenRefusal {
				t.Fatal("Claude role became an Unreal child", err)
			}
		} else if err != nil {
			t.Fatal(id, err)
		}
	}
}

// subagent model's choice is one private file; no file is Codex, and anything
// else than Codex, an OpenCode Go model or a Claude model is refused rather
// than guessed.
func TestSubagentModelFileRoundTrip(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "subagent-model.json")
	if m, err := readSubagentModel(path); err != nil || m != (subagentModel{}) {
		t.Fatal("a missing file is not Codex", m, err)
	}
	for _, want := range []subagentModel{{Version: 1, Provider: opencodego.ProviderID, Model: "deepseek-v4.1-flash"}, {Version: 1, Provider: claudeagent.ProviderID, Model: "opus"}, {Version: 1, Provider: "openai-codex"}} {
		if err := writeSubagentModel(path, want); err != nil {
			t.Fatal(err)
		}
		if got, err := readSubagentModel(path); err != nil || got != want {
			t.Fatal("round trip", got, err)
		}
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("the file is not private", info, err)
	}
	if entries, _ := os.ReadDir(directory); len(entries) != 1 {
		t.Fatal("a temporary file was left behind", entries)
	}
	for _, invalid := range []string{`{`, `{"Version":2,"Provider":"openai-codex"}`, `{"Version":1,"Provider":"opencode-go"}`, `{"Version":1,"Provider":"openai-codex","Model":"gpt-6.2-sol"}`, `{"Version":1,"Provider":"claude"}`, `{"Version":1,"Provider":"openai-codex","Extra":true}`} {
		if err := os.WriteFile(path, []byte(invalid), 0600); err != nil {
			t.Fatal(err)
		}
		if m, err := readSubagentModel(path); err == nil || m != (subagentModel{}) {
			t.Fatal("accepted", invalid)
		}
	}
	if m, err := readSubagentModel(""); err != nil || m != (subagentModel{}) {
		t.Fatal("no file configured", m, err)
	}
}

// On Flash (low, high, max) the lead and worker-max run max, worker-low
// and the collector low, and the workers between high, the default.
func TestSubagentEffortFollowsP5(t *testing.T) {
	flash := modelcatalog.Model{ID: "deepseek-v4.1-flash", Efforts: []llm.ReasoningEffort{"low", "high", "max"}, DefaultEffort: "high"}
	want := map[string]llm.ReasoningEffort{"lead": "max", "worker-max": "max", "worker-xhigh": "high", "worker-high": "high", "worker-medium": "high", "worker-low": "low", "collector": "low"}
	for name, effort := range want {
		if got := subagentEffort(name, flash); got != effort {
			t.Errorf("%s runs %s on Flash, want %s", name, got, effort)
		}
	}
	// A model with the worker's own effort runs it.
	other := modelcatalog.Model{Efforts: []llm.ReasoningEffort{"low", "medium", "high", "ultra"}, DefaultEffort: "high"}
	if subagentEffort("worker-medium", other) != "medium" || subagentEffort("lead", other) != "high" || subagentEffort("collector", other) != "low" {
		t.Fatal("efforts on another model")
	}
}

// Every template's effort on the models OpenCode Go offers: the lead and
// worker-max at the highest, worker-low and the collector at the lowest,
// another worker at its own effort, else at the default unless that is below
// it, then the highest offered effort below it.
func TestSubagentEffortOnRepresentativeModels(t *testing.T) {
	templates := []string{"lead", "worker-low", "worker-medium", "worker-high", "worker-xhigh", "worker-max", "collector"}
	tests := []struct {
		model modelcatalog.Model
		want  map[string]llm.ReasoningEffort
	}{
		// high, max; default high: medium is not offered and high is not
		// below it, xhigh is not offered and high is below it, so both run
		// high.
		{modelcatalog.Model{ID: "deepseek-v4-pro", Efforts: []llm.ReasoningEffort{"high", "max"}, DefaultEffort: "high"},
			map[string]llm.ReasoningEffort{"lead": "max", "worker-low": "high", "worker-medium": "high", "worker-high": "high", "worker-xhigh": "high", "worker-max": "max", "collector": "high"}},
		// low, medium, xhigh; default low: worker-high has no high and the
		// default is below it, so it runs the highest offered below high.
		{modelcatalog.Model{ID: "qwen3.8-max", Efforts: []llm.ReasoningEffort{"low", "medium", "xhigh"}, DefaultEffort: "low"},
			map[string]llm.ReasoningEffort{"lead": "xhigh", "worker-low": "low", "worker-medium": "medium", "worker-high": "medium", "worker-xhigh": "xhigh", "worker-max": "xhigh", "collector": "low"}},
		// max only; default max: no default is below a worker's own effort.
		{modelcatalog.Model{ID: "kimi-k3", Efforts: []llm.ReasoningEffort{"max"}, DefaultEffort: "max"},
			map[string]llm.ReasoningEffort{"lead": "max", "worker-low": "max", "worker-medium": "max", "worker-high": "max", "worker-xhigh": "max", "worker-max": "max", "collector": "max"}},
		// Every effort is offered, so the workers run their own.
		{modelcatalog.Model{ID: "gpt-6-luna", Efforts: []llm.ReasoningEffort{"low", "medium", "high", "xhigh", "max"}, DefaultEffort: "high"},
			map[string]llm.ReasoningEffort{"lead": "max", "worker-low": "low", "worker-medium": "medium", "worker-high": "high", "worker-xhigh": "xhigh", "worker-max": "max", "collector": "low"}},
		// No efforts at all: no template asks for one.
		{modelcatalog.Model{ID: "kimi-k2.7-code"},
			map[string]llm.ReasoningEffort{"lead": "", "worker-low": "", "worker-medium": "", "worker-high": "", "worker-xhigh": "", "worker-max": "", "collector": ""}},
	}
	for _, tt := range tests {
		if len(tt.want) != len(templates) {
			t.Fatalf("%s: %d templates expected, %d given", tt.model.ID, len(templates), len(tt.want))
		}
		for _, name := range templates {
			if got := subagentEffort(name, tt.model); got != tt.want[name] {
				t.Errorf("%s: %s runs %q, want %q", tt.model.ID, name, got, tt.want[name])
			}
		}
	}
}

// The Codex prompts stay as they were; on OpenCode Go they name the chosen
// model instead of the Codex models.
func TestSubagentPromptsNameTheChosenModel(t *testing.T) {
	if !strings.Contains(leadPrompt, "collector template (gpt-6-luna), never to a gpt-6.2-sol worker") || subagentPromptFor(leadPrompt, "openai-codex", "gpt-6.2-sol", "") != leadPrompt {
		t.Fatal("the Codex lead prompt changed")
	}
	retargeted := subagentPromptFor(leadPrompt, opencodego.ProviderID, "deepseek-v4.1-flash", llm.ReasoningEffortLow)
	if !strings.Contains(retargeted, "collector template (deepseek-v4.1-flash at low effort), never to a higher-effort worker") || strings.Contains(retargeted, "gpt-6") {
		t.Fatal("lead prompt on OpenCode Go", retargeted)
	}
	for _, prompt := range []string{workerPrompt, collectorPrompt} {
		if subagentPromptFor(prompt, opencodego.ProviderID, "deepseek-v4.1-flash", llm.ReasoningEffortLow) != prompt {
			t.Fatal("a prompt without model names changed")
		}
	}
	path := filepath.Join(t.TempDir(), "subagent-model.json")
	models := subagentModels(path)
	if m := models(); m.Family != "" {
		t.Fatal("Claude is told of another model without a choice", m)
	}
	if err := writeSubagentModel(path, subagentModel{Version: 1, Provider: opencodego.ProviderID, Model: "deepseek-v4.1-flash"}); err != nil {
		t.Fatal(err)
	}
	if m := models(); m.Family != "OpenCode Go" || m.Lead != "deepseek-v4.1-flash" || m.Collector != "" {
		t.Fatal("Claude's prompt does not name the chosen model", m)
	}
}

func TestSubagentModelRetargetsOperatorLeadPrompt(t *testing.T) {
	prompt := strings.ReplaceAll(leadPrompt, leadModel, "operator-gpt")
	got := subagentPromptFor(prompt, opencodego.ProviderID, "deepseek-v4.1-flash", llm.ReasoningEffortLow)
	want := strings.Replace(prompt, "collector template ("+collectorModel+"), never to a operator-gpt worker", "collector template (deepseek-v4.1-flash at low effort), never to a higher-effort worker", 1)
	if got != want || strings.Contains(got, "operator-gpt") || strings.Contains(got, collectorModel) {
		t.Fatal("the chosen Go model retained the operator's GPT model names", got)
	}
}

func TestClaudeAgentModelWithoutUnrealTemplates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subagent-model.json")
	if err := writeSubagentModel(path, subagentModel{Version: 1, Provider: claudeagent.ProviderID, Model: "opus"}); err != nil {
		t.Fatal(err)
	}
	cli, err := claudeSubagents(nil, "", path)
	if err != nil || cli.Binary != "" || len(cli.Templates) != 0 || cli.AgentModel == nil || cli.AgentModel() != "opus" {
		t.Fatal("a Host without Unreal templates lost its explicit Agent model", err)
	}
	if err := writeSubagentModel(path, subagentModel{Version: 1, Provider: claudeagent.ProviderID, Model: "sonnet"}); err != nil {
		t.Fatal(err)
	}
	if cli.AgentModel() != "sonnet" {
		t.Fatal("the Agent model did not follow the next saved choice")
	}
}

// While subagent model chooses Claude, Claude works with Claude Code's own Agent
// tool: no Unreal children, and the Agent runs the chosen Claude model.
func TestSubagentModelClaudeChoice(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude.json")
	if err := writeSubagentModel(path, subagentModel{Version: 1, Provider: claudeagent.ProviderID, Model: "opus"}); err != nil {
		t.Fatal(err)
	}
	if got := subagentProvider(path); got != claudeagent.ProviderID {
		t.Fatal("the provider on Claude", got)
	}
	if subagentsAvailable(t.Context(), agentrunner.RuntimeConfig{}, path) {
		t.Fatal("Claude was offered the Unreal children")
	}
	if got := subagentAgentModel(path)(); got != "opus" {
		t.Fatal("the Agent model on Claude", got)
	}
	agentModel := subagentAgentModel(path)
	if got := agentModel(); got != "opus" {
		t.Fatal("the Agent model is not read afresh", got)
	}
	for _, tc := range []struct {
		name  string
		model subagentModel
	}{{"codex", subagentModel{Version: 1, Provider: "openai-codex"}}, {"opencode go", subagentModel{Version: 1, Provider: opencodego.ProviderID, Model: "deepseek-v4.1-flash"}}} {
		other := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "-")+".json")
		if err := writeSubagentModel(other, tc.model); err != nil {
			t.Fatal(err)
		}
		if model := subagentAgentModel(other)(); model != "" {
			t.Fatalf("%s names an Agent model: %s", tc.name, model)
		}
	}
	if model := subagentAgentModel(filepath.Join(dir, "none.json"))(); model != "" {
		t.Fatal("no choice names an Agent model", model)
	}
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if model := subagentAgentModel(bad)(); model != "" || subagentProvider(bad) != "openai-codex" {
		t.Fatal("an unreadable choice named a model", model)
	}
}

// Without Codex the templates exist only when OpenCode Go is the chosen
// model, and then run it.
func TestDefaultSubagentsOnOpenCodeGoWithoutCodex(t *testing.T) {
	workspace := t.TempDir()
	cfg := codexServeConfiguration(workspace, "https://codex.invalid")
	cfg.Runtime.Provider.Provider = "claude"
	maximum := permission.Config{Tools: append(tool.StaticNames(), tool.ClaudeCodeName), FilesystemUnrestricted: true, NetworkUnrestricted: true, ProcessMode: permission.ProcessUnrestricted}
	cfg.PermissionModes = &permission.ModesConfig{Enabled: true, Maximum: &maximum}
	if defaultSubagents(cfg, subagentModel{}) != nil || defaultSubagents(cfg, subagentModel{Version: 1, Provider: "openai-codex"}) != nil {
		t.Fatal("templates without Codex or a chosen model")
	}
	templates := defaultSubagents(cfg, subagentModel{Version: 1, Provider: opencodego.ProviderID, Model: "deepseek-v4.1-flash"})
	lead, ok := templates["lead"]
	if !ok || len(templates) != 2+len(workerEfforts) || len(lead.Workers) != 1+len(workerEfforts) {
		t.Fatal("templates", len(templates))
	}
	for name, template := range templates {
		s := template.Runtime.Provider
		if s.Provider != opencodego.ProviderID || s.Model.ID != "deepseek-v4.1-flash" || s.Endpoint != opencodego.BaseURL || s.Auth != openCodeGoKey {
			t.Fatalf("%s: %+v", name, s)
		}
	}
	// A registered route is used as registered.
	route := openCodeGoRoute("deepseek-v4.1-flash")
	route.Endpoint = "http://127.0.0.1:9/inference/go/openai/v1"
	cfg.Providers = map[string]agentrunner.ProviderRuntime{opencodego.ProviderID: {Provider: route}}
	if lead := defaultSubagents(cfg, subagentModel{Version: 1, Provider: opencodego.ProviderID, Model: "deepseek-v4.1-flash"})["lead"]; lead.Runtime.Provider.Endpoint != route.Endpoint {
		t.Fatal("the registered route was not used", lead.Runtime.Provider)
	}
}

// subagent start --model/--effort runs on a built-in template only while its
// children run Codex; a complete runtime, a retry and an operator's
// templates (no subagent model file) keep upstream's behavior.
func TestSubagentOverrideRefusal(t *testing.T) {
	dir := t.TempDir()
	codexFile, goFile, claudeFile, badFile := filepath.Join(dir, "codex.json"), filepath.Join(dir, "go.json"), filepath.Join(dir, "claude.json"), filepath.Join(dir, "bad.json")
	if err := writeSubagentModel(codexFile, subagentModel{Version: 1, Provider: "openai-codex"}); err != nil {
		t.Fatal(err)
	}
	if err := writeSubagentModel(goFile, subagentModel{Version: 1, Provider: opencodego.ProviderID, Model: "deepseek-v4.1-flash"}); err != nil {
		t.Fatal(err)
	}
	if err := writeSubagentModel(claudeFile, subagentModel{Version: 1, Provider: claudeagent.ProviderID, Model: "opus"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(badFile, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	codex := subagent.Template{Default: &subagent.RuntimeRequest{Provider: "openai-codex", Model: leadModel, Effort: "low"}}
	onGo := subagent.Template{Default: &subagent.RuntimeRequest{Provider: opencodego.ProviderID, Model: "deepseek-v4.1-flash", Effort: "low"}}
	override := &subagent.RuntimeRequest{Effort: "high"}
	for _, tc := range []struct {
		name      string
		path      string
		template  subagent.Template
		requested *subagent.RuntimeRequest
		retry     bool
		want      string
	}{
		{"no override", goFile, codex, nil, false, ""},
		{"complete runtime", goFile, codex, &subagent.RuntimeRequest{Provider: opencodego.ProviderID, Model: "deepseek-v4.1-flash", Effort: "high"}, false, ""},
		{"operator templates", "", codex, override, false, ""},
		{"no default", goFile, subagent.Template{}, override, false, ""},
		{"retry", goFile, codex, override, true, ""},
		{"no choice", filepath.Join(dir, "none.json"), codex, override, false, ""},
		{"codex", codexFile, codex, override, false, ""},
		{"opencode go", goFile, codex, override, false, "override_unavailable"},
		{"claude", claudeFile, codex, override, false, "override_unavailable"},
		{"templates on opencode go", codexFile, onGo, override, false, "override_unavailable"},
		{"unreadable", badFile, codex, override, false, "subagent_model_unreadable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := subagentOverrideRefusal(tc.path, tc.template, tc.requested, tc.retry)
			var refusal *subagent.RuntimeError
			if tc.want == "" && err != nil || tc.want != "" && (!errors.As(err, &refusal) || refusal.Code != tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
			if tc.want == "override_unavailable" && refusal.Hint == "" {
				t.Fatal("no hint")
			}
		})
	}
	// The Claude choice refuses the override with what it means.
	var refusal *subagent.RuntimeError
	if err := subagentOverrideRefusal(claudeFile, codex, override, false); !errors.As(err, &refusal) || refusal.Hint != "subagent model で Claude を選んでいる間は --model と --effort を使えません" {
		t.Fatal("the Claude refusal", err)
	}
}

// With launcher discovery on, the templates are ready on upstream's Codex
// placeholder before a Codex login, whatever subagent model chose (each start
// binds the choice). With Codex disabled they exist only on OpenCode Go.
func TestDefaultSubagentsWithLauncherDiscovery(t *testing.T) {
	cfg := codexServeConfiguration(t.TempDir(), "https://codex.invalid")
	cfg.Runtime.Provider.Provider = "claude"
	maximum := permission.Config{Tools: append(tool.StaticNames(), tool.ClaudeCodeName), FilesystemUnrestricted: true, NetworkUnrestricted: true, ProcessMode: permission.ProcessUnrestricted}
	cfg.PermissionModes = &permission.ModesConfig{Enabled: true, Maximum: &maximum}
	cfg.Launcher = &normalLauncherConfig{Version: 1}
	flash := subagentModel{Version: 1, Provider: opencodego.ProviderID, Model: "deepseek-v4.1-flash"}
	for _, chosen := range []subagentModel{{}, flash} {
		templates := defaultSubagents(cfg, chosen)
		if len(templates) != 2+len(workerEfforts) {
			t.Fatal("templates", chosen, len(templates))
		}
		for name, template := range templates {
			if s := template.Runtime.Provider; s.Provider != "openai-codex" || s.Source != "built-in child template" {
				t.Fatalf("%v %s: %+v", chosen, name, s)
			}
		}
	}
	cfg.Launcher.CodexDisabled = true
	if defaultSubagents(cfg, subagentModel{}) != nil {
		t.Fatal("templates with Codex disabled and no OpenCode Go choice")
	}
	if s := defaultSubagents(cfg, flash)["lead"].Runtime.Provider; s.Provider != opencodego.ProviderID || s.Model.ID != "deepseek-v4.1-flash" {
		t.Fatal("the lead with Codex disabled", s)
	}
}

// A start on the chosen model records the lead's workers rebound too;
// recovery accepts exactly that, and refuses a worker changed otherwise.
func TestSubagentTemplateValidatorAcceptsReboundWorkers(t *testing.T) {
	workspace := t.TempDir()
	cfg := codexServeConfiguration(workspace, "https://codex.invalid")
	route := openCodeGoRoute("deepseek-v4.1-flash")
	cfg.Providers = map[string]agentrunner.ProviderRuntime{opencodego.ProviderID: {Provider: route, ReasoningEffort: "high"}}
	cfg.Permissions = permission.Config{Tools: []string{"read", "grep", "glob"}, ReadRoots: []string{workspace}}
	maximum := permission.Config{Tools: append(tool.StaticNames(), childToolNames...), FilesystemUnrestricted: true, NetworkUnrestricted: true, ProcessMode: permission.ProcessUnrestricted}
	cfg.PermissionModes = &permission.ModesConfig{Enabled: true, Maximum: &maximum}
	registry, err := registeredProviders(cfg.Runtime, cfg.Providers)
	if err != nil {
		t.Fatal(err)
	}
	resolve := credential.ResolverFunc(func(context.Context, credential.Reference) (credential.Material, error) {
		return credential.Material{}, nil
	})
	parent, err := permission.New(maximum)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	c, templates, err := withParentSubagents(agentrunner.RuntimeConfig{Identity: cfg.Runtime, SessionDirectory: t.TempDir(), Providers: registry, Credentials: resolve, Backends: cfg.Providers}, defaultSubagents(cfg, subagentModel{}), parent, "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	validate := childTemplateValidator(c)
	base := templates["lead"]
	backend, err := c.ProviderRuntime(opencodego.ProviderID)
	if err != nil {
		t.Fatal(err)
	}
	flash := modelcatalog.Model{ID: "deepseek-v4.1-flash", Efforts: []llm.ReasoningEffort{"low", "high", "max"}, DefaultEffort: "high"}
	chosen := base
	if chosen.Runtime, err = boundChildRuntime(c, base.Runtime, backend, sessionstore.RuntimeSelection{Model: flash.ID, Effort: subagentEffort("lead", flash)}, true, subagentEffort("collector", flash)); err != nil {
		t.Fatal(err)
	}
	chosen.Subagents = maps.Clone(base.Subagents)
	for name, worker := range base.Subagents {
		if worker.Runtime, err = boundChildRuntime(c, worker.Runtime, backend, sessionstore.RuntimeSelection{Model: flash.ID, Effort: subagentEffort(name, flash)}, true, subagentEffort("collector", flash)); err != nil {
			t.Fatal(err)
		}
		chosen.Subagents[name] = worker
	}
	if err = validate(base, chosen); err != nil {
		t.Fatal("recovery refused the chosen model's start", err)
	}
	for name, worker := range chosen.Subagents {
		s := sessionstore.SelectionFromConfiguration(worker.Runtime)
		if s == nil || s.Provider != opencodego.ProviderID || s.Effort != subagentEffort(name, flash) {
			t.Fatal("worker", name, s)
		}
	}
	// The Codex start is accepted as before.
	unchanged := base
	if unchanged.Runtime, err = boundChildRuntime(c, base.Runtime, mustProvider(t, c, "openai-codex"), sessionstore.RuntimeSelection{Model: leadModel, Effort: leadEffort}, false, ""); err != nil {
		t.Fatal(err)
	}
	if err = validate(base, unchanged); err != nil {
		t.Fatal("recovery refused a Codex start", err)
	}
	// A Codex lead may retain a separate collector and Go workers at max.
	mixed := unchanged
	mixed.Subagents = maps.Clone(base.Subagents)
	for name, worker := range mixed.Subagents {
		if subagentRole(name) != "worker" {
			continue
		}
		worker.Runtime, err = boundChildRuntime(c, worker.Runtime, backend, sessionstore.RuntimeSelection{Model: flash.ID, Effort: "max"}, false, "")
		if err != nil {
			t.Fatal(err)
		}
		mixed.Subagents[name] = worker
	}
	var collectorRuntime, leadRuntime agentrunner.RuntimeIdentity
	if err = json.Unmarshal(mixed.Subagents["collector"].Runtime, &collectorRuntime); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(mixed.Runtime, &leadRuntime); err != nil {
		t.Fatal(err)
	}
	leadRuntime.SystemPrompt = subagentCollectorPrompt(leadRuntime.SystemPrompt, collectorRuntime)
	mixed.Runtime, err = json.Marshal(leadRuntime)
	if err != nil {
		t.Fatal(err)
	}
	if err = validate(base, mixed); err != nil {
		t.Fatal("recovery refused mixed role bindings", err)
	}
	// A worker with another prompt, or a renamed worker, is refused.
	tampered := chosen
	tampered.Subagents = maps.Clone(chosen.Subagents)
	worker := tampered.Subagents["worker-high"]
	var identity agentrunner.RuntimeIdentity
	if err = json.Unmarshal(worker.Runtime, &identity); err != nil {
		t.Fatal(err)
	}
	identity.SystemPrompt = "Do something else."
	if worker.Runtime, err = json.Marshal(identity); err != nil {
		t.Fatal(err)
	}
	tampered.Subagents["worker-high"] = worker
	if validate(base, tampered) == nil {
		t.Fatal("recovery accepted a worker with another prompt")
	}
	renamed := chosen
	renamed.Subagents = maps.Clone(chosen.Subagents)
	renamed.Subagents["worker-extra"] = renamed.Subagents["worker-high"]
	delete(renamed.Subagents, "worker-high")
	if validate(base, renamed) == nil {
		t.Fatal("recovery accepted a renamed worker")
	}
	if names := slices.Sorted(maps.Keys(chosen.Subagents)); !slices.Equal(names, slices.Sorted(maps.Keys(base.Subagents))) {
		t.Fatal("workers", names)
	}
}

// The Flash lead prompt keeps the bytes children recorded before the other
// models were offered carry; a model with no effort names none.
func TestSubagentPromptPinsTheCollectorEffort(t *testing.T) {
	flash := modelcatalog.Model{ID: "deepseek-v4.1-flash", Efforts: []llm.ReasoningEffort{"low", "high", "max"}, DefaultEffort: "high"}
	prompt := subagentPromptFor(leadPrompt, opencodego.ProviderID, flash.ID, subagentEffort("collector", flash))
	want := strings.Replace(leadPrompt, codexCollectorRule, "collector template (deepseek-v4.1-flash at low effort), never to a higher-effort worker", 1)
	if prompt != want {
		t.Fatalf("the Flash lead prompt changed:\n%s", prompt)
	}
	noEffort := subagentPromptFor(leadPrompt, opencodego.ProviderID, "kimi-k2.7-code", subagentEffort("collector", modelcatalog.Model{ID: "kimi-k2.7-code"}))
	none := strings.Replace(leadPrompt, codexCollectorRule, "collector template (kimi-k2.7-code), never to a higher-effort worker", 1)
	if noEffort != none {
		t.Fatalf("the lead prompt names an effort the model has not:\n%s", noEffort)
	}
}

// A model whose list names no effort binds every template with none, and
// recovery accepts that.
func TestSubagentTemplatesBindAModelWithoutEfforts(t *testing.T) {
	workspace := t.TempDir()
	cfg := codexServeConfiguration(workspace, "https://codex.invalid")
	model := modelcatalog.Model{ID: "kimi-k2.7-code"}
	cfg.Providers = map[string]agentrunner.ProviderRuntime{opencodego.ProviderID: {Provider: openCodeGoRoute(model.ID)}}
	cfg.Permissions = permission.Config{Tools: []string{"read"}, ReadRoots: []string{workspace}}
	maximum := permission.Config{Tools: append(tool.StaticNames(), childToolNames...), FilesystemUnrestricted: true, NetworkUnrestricted: true, ProcessMode: permission.ProcessUnrestricted}
	cfg.PermissionModes = &permission.ModesConfig{Enabled: true, Maximum: &maximum}
	registry, err := registeredProviders(cfg.Runtime, cfg.Providers)
	if err != nil {
		t.Fatal(err)
	}
	resolve := credential.ResolverFunc(func(context.Context, credential.Reference) (credential.Material, error) {
		return credential.Material{}, nil
	})
	parent, err := permission.New(maximum)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	c, templates, err := withParentSubagents(agentrunner.RuntimeConfig{Identity: cfg.Runtime, SessionDirectory: t.TempDir(), Providers: registry, Credentials: resolve, Backends: cfg.Providers}, defaultSubagents(cfg, subagentModel{}), parent, "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	backend, err := c.ProviderRuntime(opencodego.ProviderID)
	if err != nil {
		t.Fatal(err)
	}
	base := templates["lead"]
	chosen := base
	if chosen.Runtime, err = boundChildRuntime(c, base.Runtime, backend, sessionstore.RuntimeSelection{Model: model.ID, Effort: subagentEffort("lead", model)}, true, subagentEffort("collector", model)); err != nil {
		t.Fatal(err)
	}
	chosen.Subagents = maps.Clone(base.Subagents)
	for name, worker := range base.Subagents {
		if worker.Runtime, err = boundChildRuntime(c, worker.Runtime, backend, sessionstore.RuntimeSelection{Model: model.ID, Effort: subagentEffort(name, model)}, true, subagentEffort("collector", model)); err != nil {
			t.Fatal(err)
		}
		chosen.Subagents[name] = worker
	}
	var lead agentrunner.RuntimeIdentity
	if err = json.Unmarshal(chosen.Runtime, &lead); err != nil || lead.ReasoningEffort != "" {
		t.Fatalf("the lead runs %q, %v", lead.ReasoningEffort, err)
	}
	for name, worker := range chosen.Subagents {
		var identity agentrunner.RuntimeIdentity
		if err = json.Unmarshal(worker.Runtime, &identity); err != nil || identity.ReasoningEffort != "" {
			t.Fatalf("%s runs %q, %v", name, identity.ReasoningEffort, err)
		}
		if s := sessionstore.SelectionFromConfiguration(worker.Runtime); s == nil || s.Effort != "" {
			t.Fatalf("%s selects %v", name, s)
		}
	}
	if err = childTemplateValidator(c)(base, chosen); err != nil {
		t.Fatal("recovery refused a model without efforts", err)
	}
}

func mustProvider(t *testing.T, c agentrunner.RuntimeConfig, id string) agentrunner.ProviderRuntime {
	t.Helper()
	p, err := c.ProviderRuntime(id)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// A child reads OpenCode Go's key from the launcher's store as its Host does,
// never creates the store, and an explicit store comes first.
func TestChildCredentialsReadTheDefaultStore(t *testing.T) {
	store := filepath.Join(privateCLIDirectory(t), "credentials")
	resolver, err := childCredentials("", store, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = resolver.Resolve(t.Context(), openCodeGoKey); !hasCredentialCode(err, "store_required") {
		t.Fatal("a missing store", err)
	}
	if _, err = os.Lstat(store); !os.IsNotExist(err) {
		t.Fatal("the child created the store", err)
	}
	storeKey(t, store, openCodeGoKey, "opencode-go-key-sensitive")
	m, err := resolver.Resolve(t.Context(), openCodeGoKey)
	if err != nil || m.Token.Reveal() != "opencode-go-key-sensitive" {
		t.Fatal("the saved key was not read", err)
	}
	explicit := filepath.Join(privateCLIDirectory(t), "credentials")
	storeKey(t, explicit, openCodeGoKey, "explicit-key-sensitive")
	if resolver, err = childCredentials(explicit, store, ""); err != nil {
		t.Fatal(err)
	}
	if m, err = resolver.Resolve(t.Context(), openCodeGoKey); err != nil || m.Token.Reveal() != "explicit-key-sensitive" {
		t.Fatal("the explicit store was not used", err)
	}
	if _, err = childCredentials("", "relative/credentials", ""); err == nil {
		t.Fatal("a relative store was accepted")
	}
}

func hasCredentialCode(err error, code string) bool {
	var e *credential.Error
	return errors.As(err, &e) && e.Code == code
}

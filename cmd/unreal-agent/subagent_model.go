package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudeagent"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/opencodego"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/subagent"
	"github.com/unreallabsai/unreal-agent/internal/buildfeatures"
)

// subagentModel is the sub-agents' model chosen with subagent model.
// The launcher keeps it in one file under STATE, so every conversation shares
// it, and each child start reads it. Without a choice, E1's GPT templates
// follow an API parent's explicit connection. Choosing Codex pins its route.
// Claude names the model Claude Code's own Agent
// sub-agents run on, instead of Unreal's children.
type subagentModel struct {
	Version  int
	Provider string
	Model    string              `json:",omitzero"`
	Roles    *subagentRoleModels `json:",omitzero"`
}

type subagentModelChoice struct {
	Provider string
	Model    string              `json:",omitzero"`
	Effort   llm.ReasoningEffort `json:",omitzero"`
}

type subagentRoleModels struct {
	Lead      *subagentModelChoice `json:"lead,omitzero"`
	Worker    *subagentModelChoice `json:"worker,omitzero"`
	Collector *subagentModelChoice `json:"collector,omitzero"`
}

// Serialize read/modify/write of the shared choice across Session requests.
var subagentModelMu sync.Mutex

var subagentRoleGroups = []string{"lead", "worker", "collector"}

func subagentRole(name string) string {
	if strings.HasPrefix(name, "worker-") {
		return "worker"
	}
	return name
}

func (m subagentModel) choice(name string) subagentModelChoice {
	if m.Roles != nil {
		var chosen *subagentModelChoice
		switch subagentRole(name) {
		case "lead":
			chosen = m.Roles.Lead
		case "worker":
			chosen = m.Roles.Worker
		case "collector":
			chosen = m.Roles.Collector
		}
		if chosen != nil {
			return *chosen
		}
	}
	return subagentModelChoice{Provider: m.Provider, Model: m.Model}
}

func (m *subagentModel) setRole(role string, choice subagentModelChoice) {
	m.Version = 2
	if m.Roles == nil {
		m.Roles = &subagentRoleModels{}
	}
	switch role {
	case "lead":
		m.Roles.Lead = &choice
	case "worker":
		m.Roles.Worker = &choice
	case "collector":
		m.Roles.Collector = &choice
	}
}

func (m subagentModel) valid() bool {
	legacy := m.Provider == "openai-codex" && m.Model == "" ||
		(m.Provider == opencodego.ProviderID || m.Provider == claudeagent.ProviderID) && m.Model != ""
	if m.Version == 1 {
		return legacy && m.Roles == nil
	}
	if m.Version != 2 || m.Roles == nil || !(legacy || m.Provider == "" && m.Model == "") {
		return false
	}
	for _, role := range subagentRoleGroups {
		c := m.choice(role)
		if c.Provider == "" && c.Model == "" && c.Effort == "" {
			continue
		}
		if c.Provider == "" {
			return false
		}
		if c.Provider != "openai-codex" && c.Model == "" {
			return false
		}
		if c.Effort != "" && !slices.Contains(workerEfforts, c.Effort) {
			return false
		}
	}
	return true
}

func readSubagentModel(path string) (subagentModel, error) {
	var m subagentModel
	if path == "" {
		return m, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return m, err
	}
	if json.Unmarshal(data, &m, json.RejectUnknownMembers(true)) != nil || !m.valid() {
		return subagentModel{}, errors.New("invalid sub-agent model file")
	}
	return m, nil
}

// writeSubagentModel replaces the file whole, so a child start never reads a
// partial choice.
func writeSubagentModel(path string, m subagentModel) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".subagent-model-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// subagentEffort is the effort a built-in template runs at on the sub-agents'
// model (docs/opencode-go.md): the lead and worker-max at the model's highest,
// worker-low and the collector at its lowest, another worker at its own effort
// when the model has it, otherwise at the model's default, unless that default
// is below its own effort: then the highest effort the model offers below it.
// On qwen3.8-max (low, medium, xhigh; default low) worker-high runs medium.
func subagentEffort(name string, m modelcatalog.Model) llm.ReasoningEffort {
	var offered []llm.ReasoningEffort
	for _, e := range workerEfforts {
		if slices.Contains(m.Efforts, e) {
			offered = append(offered, e)
		}
	}
	if len(offered) == 0 {
		return m.DefaultEffort
	}
	switch name {
	case "lead", "worker-max":
		return offered[len(offered)-1]
	case "worker-low", "collector":
		return offered[0]
	}
	own := llm.ReasoningEffort(strings.TrimPrefix(name, "worker-"))
	if slices.Contains(offered, own) {
		return own
	}
	if d, o := slices.Index(workerEfforts, m.DefaultEffort), slices.Index(workerEfforts, own); d >= 0 && o >= 0 && d < o {
		for i := len(offered) - 1; i >= 0; i-- {
			if slices.Index(workerEfforts, offered[i]) < o {
				return offered[i]
			}
		}
	}
	return m.DefaultEffort
}

// codexCollectorRule names the Codex models in the lead's prompt.
const codexCollectorRule = "collector template (" + collectorModel + "), never to a " + leadModel + " worker"

// subagentPromptFor is a built-in prompt for the model it runs: on OpenCode
// Go every template runs the one chosen model, the collector at its effort on
// that model; a model with no effort names none.
func subagentPromptFor(prompt, provider, model string, collector llm.ReasoningEffort) string {
	if provider != opencodego.ProviderID {
		return prompt
	}
	rule := "collector template (" + model + ")"
	if collector != "" {
		rule = "collector template (" + model + " at " + string(collector) + " effort)"
	}
	// Operator defaults can rename the GPT lead before subagent model is chosen.
	// Keep the shared product prompt and replace its collector/lead rule too.
	prefix, _, _ := strings.Cut(codexCollectorRule, leadModel)
	before, after, found := strings.Cut(prompt, prefix)
	if found {
		if _, tail, ok := strings.Cut(after, " worker"); ok {
			return before + rule + ", never to a higher-effort worker" + tail
		}
	}
	return prompt
}

func subagentCollectorPrompt(prompt string, collector agentrunner.RuntimeIdentity) string {
	before, rest, ok := strings.Cut(prompt, "collector template (")
	if !ok {
		return prompt
	}
	_, tail, ok := strings.Cut(rest, " worker")
	if !ok {
		return prompt
	}
	label := collector.Provider.Model.ID
	if collector.ReasoningEffort != "" {
		label += " at " + string(collector.ReasoningEffort.Normalize()) + " effort"
	}
	return before + "collector template (" + label + "), never to a worker" + tail
}

// subagentProvider is the provider the built-in children run on: OpenCode Go
// or Claude when subagent model chose it, otherwise Codex. An unreadable choice
// counts as Codex here, as at Host start; each child start then reports it.
// Claude has no built-in children: Claude's own Agent runs the sub-agents.
func subagentProvider(path string, names ...string) string {
	name := "lead"
	if len(names) > 0 {
		name = names[0]
	}
	if m, err := readSubagentModel(path); err == nil {
		if c := m.choice(name); c.Provider != "" {
			return c.Provider
		}
	}
	return "openai-codex"
}

// subagentAgentModel names the Claude model chosen for Claude Code's Agent
// tool, from the file at each turn; "" for another choice.
func subagentAgentModel(path string) func() string {
	return func() string {
		m, err := readSubagentModel(path)
		if err != nil {
			return ""
		}
		model := m.choice("lead").Model
		for _, role := range subagentRoleGroups {
			c := m.choice(role)
			if c.Provider != claudeagent.ProviderID || c.Model != model {
				return ""
			}
		}
		return model
	}
}

// subagentsAvailable reports whether Claude is offered the built-in children
// this turn: while the provider they run on is available. OpenCode Go counts
// while it is registered and its saved key resolves. Its model list is a
// network read that a Claude turn must not wait on, nor lose the children
// over, and a child start reads the list itself. The Claude choice wants no
// Unreal children: Claude's own Agent tool runs the sub-agents instead.
func subagentsAvailable(ctx context.Context, runtime agentrunner.RuntimeConfig, path string) bool {
	if m, err := readSubagentModel(path); err == nil && m.Roles != nil {
		for _, role := range subagentRoleGroups {
			id := m.choice(role).Provider
			if id == "" {
				id = "openai-codex"
			}
			if subagentRoleAvailable(ctx, runtime, id) {
				return true
			}
		}
		return false
	}
	id := subagentProvider(path)
	return subagentRoleAvailable(ctx, runtime, id)
}

func subagentRoleAvailable(ctx context.Context, runtime agentrunner.RuntimeConfig, id string) bool {
	if id == claudeagent.ProviderID {
		return false
	}
	if id == opencodego.ProviderID {
		return runtime.CheckProvider(ctx, id) == nil
	}
	return providerAvailable(ctx, runtime, id)
}

func subagentRoles(runtime agentrunner.RuntimeConfig, path string, templates map[string]subagent.Template) func(context.Context) []claudeagent.SubagentRole {
	return func(ctx context.Context) []claudeagent.SubagentRole {
		m, err := readSubagentModel(path)
		if err != nil || m.Roles == nil && (m.Provider == "" || m.Provider == claudeagent.ProviderID) {
			return nil
		}
		roles := make([]claudeagent.SubagentRole, 0, len(subagentRoleGroups))
		for _, group := range subagentRoleGroups {
			c := m.choice(group)
			if c.Provider == "" {
				c.Provider = "openai-codex"
			}
			role := claudeagent.SubagentRole{Role: group, Provider: c.Provider, Model: c.Model, Effort: string(c.Effort)}
			for name := range templates {
				if subagentRole(name) == group {
					role.Templates = append(role.Templates, name)
				}
			}
			slices.Sort(role.Templates)
			if c.Provider == claudeagent.ProviderID {
				role.Available = providerAvailable(ctx, runtime, c.Provider)
			} else {
				role.Available = len(role.Templates) > 0 && subagentRoleAvailable(ctx, runtime, c.Provider)
			}
			if role.Model == "" {
				if group == "collector" {
					role.Model = collectorModel
				} else {
					role.Model = leadModel
				}
			}
			if role.Effort == "" && c.Provider != claudeagent.ProviderID {
				if c.Provider == opencodego.ProviderID || group == "worker" {
					role.Effort = "template default"
				} else if group == "lead" {
					role.Effort = string(leadEffort)
				} else if group == "collector" {
					role.Effort = string(collectorEffort)
				}
			}
			roles = append(roles, role)
		}
		return roles
	}
}

// subagentOverrideRefusal refuses subagent start --model/--effort on a
// built-in template unless its children run Codex. Upstream's override keeps
// the template's provider and skips the subagent model binding, so under OpenCode
// Go it would run Codex, or the model chosen at Host start; under Claude the
// template would start a child Unreal must not start. A retry of a start
// already accepted is left to StartChild.
func subagentOverrideRefusal(path string, template subagent.Template, requested *subagent.RuntimeRequest, retry bool, names ...string) error {
	name := "lead"
	if len(names) > 0 {
		name = names[0]
	}
	if !buildfeatures.OpenCodeGoEnabled && !retry {
		if requested != nil && buildfeatures.Disabled(requested.Provider) {
			return disabledChildError()
		}
		if requested == nil || requested.Provider == "" {
			if template.Default != nil && buildfeatures.Disabled(template.Default.Provider) {
				return disabledChildError()
			}
			if path != "" && template.Default != nil {
				if m, err := readSubagentModel(path); err == nil && buildfeatures.Disabled(m.choice(name).Provider) {
					return disabledChildError()
				}
			}
		}
	}
	if requested == nil || requested.Provider != "" || path == "" || template.Default == nil || retry {
		return nil
	}
	m, err := readSubagentModel(path)
	if err != nil {
		return &subagent.RuntimeError{Code: "subagent_model_unreadable"}
	}
	choice := m.choice(name)
	if buildfeatures.Disabled(choice.Provider) {
		return disabledChildError()
	}
	if choice.Provider == claudeagent.ProviderID {
		return &subagent.RuntimeError{Code: "override_unavailable", Hint: "subagent model で Claude を選んでいる間は --model と --effort を使えません"}
	}
	if choice.Provider == opencodego.ProviderID {
		return &subagent.RuntimeError{Code: "override_unavailable", Hint: "subagent model で OpenCode Go を選んでいる間は --model と --effort を使えません。子は subagent model のモデルを役割ごとの effort で使います。モデルを変えるには subagent model で選び直してください"}
	}
	if template.Default.Provider != "openai-codex" && !(m.Roles != nil && choice.Provider == "openai-codex") {
		return &subagent.RuntimeError{Code: "override_unavailable", Hint: "この Host の組み込みテンプレートは Codex で作られていないため、--model と --effort を使えません"}
	}
	return nil
}

// subagentModels names the sub-agents' model for Claude's prompt, from the
// file at each turn.
func subagentModels(path string) func() claudeagent.SubagentModels {
	return func() claudeagent.SubagentModels {
		m, err := readSubagentModel(path)
		if err != nil || m.Provider != opencodego.ProviderID {
			return claudeagent.SubagentModels{}
		}
		return claudeagent.SubagentModels{Family: modelcatalog.ProviderName(m.Provider), Lead: m.Model}
	}
}

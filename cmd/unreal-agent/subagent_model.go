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

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudeagent"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/opencodego"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/subagent"
)

// subagentModel holds a Session's role selections or the shared defaults.
// With no choice, built-in GPT templates follow an API parent's explicit
// connection. Choosing Codex pins its route.
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
	Fast     bool                `json:",omitzero"`
}

type subagentRoleModels struct {
	Lead      *subagentModelChoice `json:"lead,omitzero"`
	Worker    *subagentModelChoice `json:"worker,omitzero"`
	Collector *subagentModelChoice `json:"collector,omitzero"`
}

var subagentRoleGroups = []string{"lead", "worker", "collector"}

func subagentRole(name string) string {
	return subagent.RoleGroup(name)
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
		if c.Provider == "" && c.Model == "" && c.Effort == "" && !c.Fast {
			continue
		}
		if c.Provider == "" || c.Fast && c.Provider != "openai-codex" {
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

// migrateSubagentModel seeds shared state from the first launched repo with a
// saved choice. Keep the original bytes and file, and never replace a shared
// choice, including when another workspace launches concurrently.
func migrateSubagentModel(legacy, shared string) error {
	if legacy == shared {
		return nil
	}
	if _, err := os.Lstat(shared); !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	// The shared directory is also needed for future model set commands when
	// neither a key nor a legacy model choice has been saved yet.
	if err := os.MkdirAll(filepath.Dir(shared), 0700); err != nil {
		return err
	}
	data, err := os.ReadFile(legacy)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(shared), ".subagent-model-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Link(f.Name(), shared); errors.Is(err, fs.ErrExist) {
		return nil
	}
	return err
}

// writeSubagentModel replaces the file whole, so a child start never reads a
// partial choice.
func writeSubagentModel(path string, m subagentModel) error {
	temporary, err := prepareSubagentModel(path, m)
	if err != nil {
		return err
	}
	defer os.Remove(temporary)
	return os.Rename(temporary, path)
}

func prepareSubagentModel(path string, m subagentModel) (string, error) {
	data, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".subagent-model-*")
	if err != nil {
		return "", err
	}
	_, err = f.Write(data)
	if err = errors.Join(err, f.Close()); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
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

// subagentsAvailable reports whether Claude is offered the built-in children
// this turn: while the provider they run on is available. OpenCode Go counts
// while it is registered and its saved key resolves. Its model list is a
// network read that a Claude turn must not wait on, nor lose the children
// over, and a child start reads the list itself.
func subagentsAvailable(ctx context.Context, runtime agentrunner.RuntimeConfig, m subagentModel) bool {
	checked := make(map[string]bool)
	for _, role := range subagentRoleGroups {
		id := m.choice(role).Provider
		if id == "" {
			id = "openai-codex"
		}
		if !checked[id] && subagentRoleAvailable(ctx, runtime, id) {
			return true
		}
		checked[id] = true
	}
	return false
}

func subagentRoleAvailable(ctx context.Context, runtime agentrunner.RuntimeConfig, id string) bool {
	if id == opencodego.ProviderID {
		return runtime.CheckProvider(ctx, id) == nil
	}
	return providerAvailable(ctx, runtime, id)
}

func subagentRoles(runtime agentrunner.RuntimeConfig, scope subagentModelScope, templates map[string]subagent.Template) func(context.Context) []claudeagent.SubagentRole {
	return func(ctx context.Context) []claudeagent.SubagentRole {
		m, err := scope.read(ctx, "")
		if err != nil || scope.defaultFile != "" && m.Version == 0 {
			return nil
		}
		roles := make([]claudeagent.SubagentRole, 0, len(subagentRoleGroups))
		for _, group := range subagentRoleGroups {
			var names []string
			for name := range templates {
				if subagentRole(name) == group {
					names = append(names, name)
				}
			}
			if len(names) == 0 && len(templates) > 0 {
				continue
			}
			slices.Sort(names)
			c := m.choice(group)
			if scope.defaultFile == "" && len(names) > 0 {
				if selected := sessionstore.SelectionFromConfiguration(templates[names[0]].Runtime); selected != nil {
					c.Provider, c.Model, c.Effort = selected.Provider, selected.Model, selected.Effort
				}
			}
			if c.Provider == "" {
				c.Provider = "openai-codex"
			}
			role := claudeagent.SubagentRole{Role: group, Provider: c.Provider, Model: c.Model, Effort: string(c.Effort), Templates: names, Available: len(names) > 0 && subagentRoleAvailable(ctx, runtime, c.Provider)}
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

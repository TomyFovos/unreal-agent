package main

import (
	"maps"
	"slices"
	"strings"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudeagent"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/openaicodex"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/opencodego"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/profile"
	"github.com/unreallabsai/unreal-agent/harness/provider"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"github.com/unreallabsai/unreal-agent/internal/buildfeatures"
)

// Operator-selected defaults reuse the product's lead/worker prompts and tools.
// Workers retain their template effort; collector retains its product model.
type subagentDefaults struct {
	Provider string
	Model    string
	Effort   llm.ReasoningEffort
}

const (
	leadModel        = agentrunner.LeadModel
	leadEffort       = agentrunner.LeadEffort
	collectorModel   = agentrunner.CollectorModel
	collectorEffort  = agentrunner.CollectorEffort
	progressGuidance = agentrunner.ProgressGuidance
	finishGuidance   = agentrunner.FinishGuidance
	leadPrompt       = agentrunner.LeadPrompt
	workerPrompt     = agentrunner.WorkerPrompt
	collectorPrompt  = agentrunner.CollectorPrompt
)

var workerEfforts = slices.Clone(agentrunner.WorkerEfforts)
var childToolNames = slices.Clone(agentrunner.ChildToolNames)

// defaultSubagents gives a normal Host the lead and worker templates when the
// operator configured none and a default or saved role connection is registered.
// They follow the Session's permission mode, bounded by its maximum.
func defaultSubagents(config serveConfiguration, chosen subagentModel, roleSettings ...bool) map[string]childTemplate {
	if config.DisableSubagents {
		return nil
	}
	route, ok := codexSelection(config)
	model, effort := leadModel, leadEffort
	if selected := config.SubagentDefaults; selected != nil {
		if config.Runtime.Provider.Provider == selected.Provider {
			route, ok = config.Runtime.Provider, true
		} else {
			p, found := config.Providers[selected.Provider]
			route, ok = p.Provider, found
		}
		if !ok {
			return nil
		}
		if selected.Model != "" {
			model = selected.Model
		}
		if selected.Effort != "" {
			effort = selected.Effort
		}
	}
	hasAPI := config.Runtime.Provider.Provider == "openai" || config.Providers["openai"].Provider.Provider == "openai"
	if !ok && (hasAPI || config.Launcher != nil && !config.Launcher.CodexDisabled && (config.Launcher.DiscoverProviders == nil || *config.Launcher.DiscoverProviders)) {
		// Keep templates ready for a later login. Runtime resolution binds the
		// authenticated route at child start (or the model chosen with
		// subagent model); Claude advertises them only while that route is available.
		route = provider.Selection{Version: 1, Provider: "openai-codex", Endpoint: openaicodex.BaseURL, Auth: credential.Reference{Provider: "openai-codex", Method: credential.OAuth, ID: "external-codex"}, MaxAttempts: openaicodex.DefaultMaxAttempts, Source: "built-in child template"}
		ok = true
	}
	if !ok {
		for _, role := range subagentRoleGroups {
			choice := chosen.choice(role)
			if choice.Provider == opencodego.ProviderID {
				route, ok = openCodeGoSelection(config, choice.Model), true
			} else if choice.Provider != "" {
				if config.Runtime.Provider.Provider == choice.Provider {
					route, ok = config.Runtime.Provider, true
				} else if backend, registered := config.Providers[choice.Provider]; registered {
					route, ok = backend.Provider, true
				}
			}
			if ok {
				break
			}
		}
	}
	if !ok && len(roleSettings) > 0 && roleSettings[0] {
		// A Session can select a role without a Codex connection. Prepare
		// templates from a registration and let each start bind its selection.
		ids := append([]string{config.Runtime.Provider.Provider}, slices.Sorted(maps.Keys(config.Providers))...)
		for _, id := range ids {
			if id == "" || id == loginProvider || buildfeatures.Disabled(id) {
				continue
			}
			if id == config.Runtime.Provider.Provider {
				route = config.Runtime.Provider
			} else {
				route = config.Providers[id].Provider
			}
			ok = true
			break
		}
	}
	if !ok || config.PermissionModes == nil || !config.PermissionModes.Enabled || config.PermissionModes.Maximum == nil {
		return nil
	}
	maximum := *config.PermissionModes.Maximum
	baseline := config.Permissions
	policy := func(tools []string) permission.Config {
		p := maximum
		p.Tools = nil
		for _, name := range maximum.Tools {
			if name != tool.ClaudeCodeName && !slices.Contains(childToolNames, name) {
				p.Tools = append(p.Tools, name)
			}
		}
		p.Tools = append(p.Tools, tools...)
		return p
	}
	runtime := func(model string, effort llm.ReasoningEffort, prompt string) agentrunner.RuntimeIdentity {
		selection := route
		var claude *claudeagent.Config
		if claudeagent.IsProvider(selection.Provider) {
			claude = config.Providers[selection.Provider].Claude
			if selection.Provider == config.Runtime.Provider.Provider {
				claude = config.Runtime.Claude
			}
		}
		if selection.Provider == opencodego.ProviderID && chosen.Provider == opencodego.ProviderID && chosen.Roles == nil {
			// Each start binds the effort and the prompt to the chosen model
			// (childRuntimeResolver).
			model = chosen.Model
		}
		selection.Model.ID = model
		return agentrunner.RuntimeIdentity{Version: 1, Provider: selection, Profile: profile.Default(), Workspace: config.Runtime.Workspace, SystemPrompt: prompt, ReasoningEffort: effort, Claude: claude}
	}
	collectorID, collectorLevel := collectorModel, collectorEffort
	claudeDefaults := config.SubagentDefaults != nil && claudeagent.IsProvider(route.Provider)
	if claudeDefaults {
		collectorID, collectorLevel = model, effort
	}
	workers := map[string]childTemplate{}
	for _, tier := range workerEfforts {
		workerLevel := tier
		if claudeDefaults {
			workerLevel = effort
		}
		workers["worker-"+string(tier)] = childTemplate{Runtime: runtime(model, workerLevel, workerPrompt), Permissions: policy([]string{"Finish", "SendParent"}), Baseline: &baseline, Pinned: true}
	}
	workers["collector"] = childTemplate{Runtime: runtime(collectorID, collectorLevel, collectorPrompt), Permissions: policy([]string{"Finish", "SendParent"}), Baseline: &baseline, Pinned: true}
	leadInstructions := strings.ReplaceAll(leadPrompt, leadModel, model)
	if collectorID != collectorModel {
		leadInstructions = subagentCollectorPrompt(leadInstructions, workers["collector"].Runtime)
	}
	templates := map[string]childTemplate{"lead": {Runtime: runtime(model, effort, leadInstructions), Permissions: policy(childToolNames), Baseline: &baseline, Pinned: true, Workers: workers}}
	for name, worker := range workers {
		templates[name] = worker
	}
	return templates
}

// codexSelection is the registered Codex route, as the initial provider or a
// backend.
func codexSelection(config serveConfiguration) (selection provider.Selection, ok bool) {
	if config.Runtime.Provider.Provider == "openai-codex" {
		return config.Runtime.Provider, true
	}
	if p, found := config.Providers["openai-codex"]; found && p.Provider.Provider == "openai-codex" {
		return p.Provider, true
	}
	return selection, false
}

// openCodeGoSelection is the registered OpenCode Go route, or the one
// discovery registers once its key is saved.
func openCodeGoSelection(config serveConfiguration, model string) provider.Selection {
	if config.Runtime.Provider.Provider == opencodego.ProviderID {
		return config.Runtime.Provider
	}
	if p, found := config.Providers[opencodego.ProviderID]; found && p.Provider.Provider == opencodego.ProviderID {
		return p.Provider
	}
	return openCodeGoRoute(model)
}

// withChildTools lets a maximum cover the subagent tools the default
// templates use.
func withChildTools(c permission.Config) permission.Config {
	c.Tools = slices.Clone(c.Tools)
	for _, name := range childToolNames {
		if !slices.Contains(c.Tools, name) {
			c.Tools = append(c.Tools, name)
		}
	}
	return c
}

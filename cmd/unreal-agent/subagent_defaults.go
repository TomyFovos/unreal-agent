package main

import (
	"slices"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/profile"
	"github.com/unreallabsai/unreal-agent/harness/provider"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

// The implementers Claude designs for: the lead runs gpt-6.1-sol at ultra and
// starts its own workers, which never run at ultra (the user's structure).
const (
	leadModel  = "gpt-6.1-sol"
	leadEffort = llm.ReasoningEffortUltra
)

var workerEfforts = []llm.ReasoningEffort{llm.ReasoningEffortLow, llm.ReasoningEffortMedium, llm.ReasoningEffortHigh, llm.ReasoningEffortXHigh, llm.ReasoningEffortMax}

// childToolNames are the subagent tools: a child's own, then a parent's.
var childToolNames = []string{"Finish", "SendParent", "SubagentStart", "SubagentSend", "SubagentCancel", "SubagentApprove"}

const leadPrompt = `You are the implementation lead. Your parent agent is the designer; its task message is your specification.
Plan the work and delegate implementation and test runs to workers with SubagentStart. Worker templates: worker-low, worker-medium, worker-high, worker-xhigh, worker-max; pick the effort each task needs. Workers cannot run at ultra.
Give each worker a precise, self-contained task, review what it reports, and integrate the results. Do small steps yourself, such as reading a file or running git log: a worker costs a whole new context, so delegate only work that takes real effort or runs in parallel.
When the Session asks for approval, workers ask you, not the user: decide with SubagentApprove. Ask your parent with SendParent when the specification is unclear.
When the work is done and verified, call Finish with what changed, the tests you ran and their results, and any blockers.`

const workerPrompt = `You are a worker. Do the task your parent gives you: implement it, run the commands it names (tests, builds), wait for them to finish, and report.
Don't change the design or touch unrelated code. Ask your parent with SendParent if the task is unclear.
Call Finish with the changed files, the tests you ran and their results (exit codes, failing tests, key log lines), and any blockers.`

// defaultSubagents gives a normal Host the lead and worker templates when the
// operator configured none and Codex is registered. They follow the Session's
// permission mode, bounded by its maximum; nil means no subagents.
func defaultSubagents(config serveConfiguration) map[string]childTemplate {
	codex, ok := codexSelection(config)
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
	runtime := func(effort llm.ReasoningEffort, prompt string) agentrunner.RuntimeIdentity {
		selection := codex
		selection.Model.ID = leadModel
		return agentrunner.RuntimeIdentity{Version: 1, Provider: selection, Profile: profile.Default(), Workspace: config.Runtime.Workspace, SystemPrompt: prompt, ReasoningEffort: effort}
	}
	workers := map[string]childTemplate{}
	for _, effort := range workerEfforts {
		workers["worker-"+string(effort)] = childTemplate{Runtime: runtime(effort, workerPrompt), Permissions: policy([]string{"Finish", "SendParent"}), Baseline: &baseline, Pinned: true}
	}
	templates := map[string]childTemplate{"lead": {Runtime: runtime(leadEffort, leadPrompt), Permissions: policy(childToolNames), Baseline: &baseline, Pinned: true, Workers: workers}}
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

package main

import (
	"slices"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/openaicodex"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/profile"
	"github.com/unreallabsai/unreal-agent/harness/provider"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

// The implementers Claude designs for: the lead runs gpt-6.1-sol at max and
// starts its own workers at low through max (the user's structure).
const (
	leadModel  = "gpt-6.1-sol"
	leadEffort = llm.ReasoningEffortMax
	// The collector takes the low-difficulty tasks listed in collectorRule:
	// gpt-6-luna is capable enough for them and far cheaper than gpt-6.1-sol.
	collectorModel  = "gpt-6-luna"
	collectorEffort = llm.ReasoningEffortMedium
)

var workerEfforts = []llm.ReasoningEffort{llm.ReasoningEffortLow, llm.ReasoningEffortMedium, llm.ReasoningEffortHigh, llm.ReasoningEffortXHigh, llm.ReasoningEffortMax}

// childToolNames are the subagent tools: a child's own, then a parent's.
var childToolNames = []string{"Finish", "SendParent", "SubagentStart", "SubagentSend", "SubagentCancel", "SubagentApprove"}

const progressGuidance = `Send progress, acknowledgements and completion estimates with SendParent progress: true; they wait for the parent's next turn without waking it. For questions, decisions or approvals, omit progress (false by default) to wake the parent. Do not repeat the same content across messages, progress updates and Finish.`

const finishGuidance = `In Finish summary, let the parent decide without rereading: changes (commit SHA/version, main files and points); tests/results (key failure lines); decisions for the parent; unverified items. Cite paths, lines or commits; omit long source text and full diffs.`

const leadPrompt = `You are the implementation lead; your parent's task is the specification.
Delegate implementation and test runs with SubagentStart, choosing worker-low, worker-medium, worker-high, worker-xhigh or worker-max as needed. Collector tasks must go to the collector template (gpt-6-luna), never to a gpt-6.1-sol worker: URL/API fields; CI/run status and failing log lines; file listings/searches; values, counts or sections from files/logs; read-only git summaries; disk/process/environment checks; summaries of existing reports/documents. Batch these questions per collector. Do not ask collectors to save files; use a worker or save them yourself. You may run a single read-only command yourself when it directly answers a question.
Implement the specification and nothing more; add no unrequested limits, checks, abstractions or features. Review workers for the same before integrating results.
Run independent tasks in parallel and continue other work while results arrive. Give workers precise, self-contained tasks beginning with a one-line title; review and integrate their results. Do small steps yourself when delegation adds needless overhead.
Answer worker approval requests with SubagentApprove. Ask your parent with SendParent when the specification is unclear.
` + progressGuidance + `
After verification, call Finish alone with status, summary, changedFiles, tests and blockers.
` + finishGuidance

const workerPrompt = `You are a worker. Complete your parent's task and named commands (tests/builds), waiting for their results.
Implement only the task: no design changes, unrelated edits or unrequested limits, checks, abstractions or features. Ask your parent with SendParent if unclear.
` + progressGuidance + `
Call Finish alone after operations finish: status, summary, changedFiles, tests (exit codes, failures and key log lines), blockers.
` + finishGuidance

const collectorPrompt = `You are a collector for low-difficulty questions: URL/API retrieval, CI/run status, file listings/searches, file/log extraction, git/document summaries, disk/process checks. Read-only URL and API requests are allowed.
Return exact values, paths, line numbers and brief quotes. Do not edit or save files, commit or change state; report any requested mutation to your parent.
` + progressGuidance + `
After operations finish, call Finish alone: put the answers in summary, set status to completed or failed, changedFiles to [], and report tests and blockers in their fields (use [] when absent).
Cite source paths, lines or commits with your answers.`

// defaultSubagents gives a normal Host the lead and worker templates when the
// operator configured none and Codex is registered or discovery is enabled.
// They follow the Session's permission mode, bounded by its maximum.
func defaultSubagents(config serveConfiguration) map[string]childTemplate {
	codex, ok := codexSelection(config)
	if !ok && config.Launcher != nil && !config.Launcher.CodexDisabled && (config.Launcher.DiscoverProviders == nil || *config.Launcher.DiscoverProviders) {
		// Keep templates ready for a later login. Runtime resolution binds the
		// authenticated route at child start; Claude advertises them only while
		// that route is available.
		codex = provider.Selection{Version: 1, Provider: "openai-codex", Endpoint: openaicodex.BaseURL, Auth: credential.Reference{Provider: "openai-codex", Method: credential.OAuth, ID: "external-codex"}, MaxAttempts: openaicodex.DefaultMaxAttempts, Source: "built-in child template"}
		ok = true
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
		selection := codex
		selection.Model.ID = model
		return agentrunner.RuntimeIdentity{Version: 1, Provider: selection, Profile: profile.Default(), Workspace: config.Runtime.Workspace, SystemPrompt: prompt, ReasoningEffort: effort}
	}
	workers := map[string]childTemplate{}
	for _, effort := range workerEfforts {
		workers["worker-"+string(effort)] = childTemplate{Runtime: runtime(leadModel, effort, workerPrompt), Permissions: policy([]string{"Finish", "SendParent"}), Baseline: &baseline, Pinned: true}
	}
	workers["collector"] = childTemplate{Runtime: runtime(collectorModel, collectorEffort, collectorPrompt), Permissions: policy([]string{"Finish", "SendParent"}), Baseline: &baseline, Pinned: true}
	templates := map[string]childTemplate{"lead": {Runtime: runtime(leadModel, leadEffort, leadPrompt), Permissions: policy(childToolNames), Baseline: &baseline, Pinned: true, Workers: workers}}
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

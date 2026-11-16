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
	"github.com/unreallabsai/unreal-agent/harness/subagent"
	"github.com/unreallabsai/unreal-agent/internal/buildfeatures"
)

// subagentModel is the sub-agents' model chosen with /model-sub.
// The launcher keeps it in one file under STATE, so every conversation shares
// it, and each child start reads it. Without a choice, E1's GPT templates
// follow an API parent's explicit connection. Choosing Codex pins its route.
// Claude names the model Claude Code's own Agent
// sub-agents run on, instead of Unreal's children.
type subagentModel struct {
	Version  int
	Provider string
	Model    string `json:",omitzero"`
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
	if json.Unmarshal(data, &m, json.RejectUnknownMembers(true)) != nil || m.Version != 1 ||
		!(m.Provider == "openai-codex" && m.Model == "" ||
			(m.Provider == opencodego.ProviderID || m.Provider == claudeagent.ProviderID) && m.Model != "") {
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
	// Operator defaults can rename the GPT lead before /model-sub is chosen.
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

// subagentProvider is the provider the built-in children run on: OpenCode Go
// or Claude when /model-sub chose it, otherwise Codex. An unreadable choice
// counts as Codex here, as at Host start; each child start then reports it.
// Claude has no built-in children: Claude's own Agent runs the sub-agents.
func subagentProvider(path string) string {
	if m, err := readSubagentModel(path); err == nil && (m.Provider == opencodego.ProviderID || m.Provider == claudeagent.ProviderID) {
		return m.Provider
	}
	return "openai-codex"
}

// subagentAgentModel names the Claude model chosen for Claude Code's Agent
// tool, from the file at each turn; "" for another choice.
func subagentAgentModel(path string) func() string {
	return func() string {
		m, err := readSubagentModel(path)
		if err != nil || m.Provider != claudeagent.ProviderID {
			return ""
		}
		return m.Model
	}
}

// subagentsAvailable reports whether Claude is offered the built-in children
// this turn: while the provider they run on is available. OpenCode Go counts
// while it is registered and its saved key resolves. Its model list is a
// network read that a Claude turn must not wait on, nor lose the children
// over, and a child start reads the list itself. The Claude choice wants no
// Unreal children: Claude's own Agent tool runs the sub-agents instead.
func subagentsAvailable(ctx context.Context, runtime agentrunner.RuntimeConfig, path string) bool {
	id := subagentProvider(path)
	if id == claudeagent.ProviderID {
		return false
	}
	if id == opencodego.ProviderID {
		return runtime.CheckProvider(ctx, id) == nil
	}
	return providerAvailable(ctx, runtime, id)
}

// subagentOverrideRefusal refuses subagent start --model/--effort on a
// built-in template unless its children run Codex. Upstream's override keeps
// the template's provider and skips the /model-sub binding, so under OpenCode
// Go it would run Codex, or the model chosen at Host start; under Claude the
// template would start a child Unreal must not start. A retry of a start
// already accepted is left to StartChild.
func subagentOverrideRefusal(path string, template subagent.Template, requested *subagent.RuntimeRequest, retry bool) error {
	if !buildfeatures.OpenCodeGoEnabled && !retry {
		if requested != nil && buildfeatures.Disabled(requested.Provider) {
			return disabledChildError()
		}
		if requested == nil || requested.Provider == "" {
			if template.Default != nil && buildfeatures.Disabled(template.Default.Provider) {
				return disabledChildError()
			}
			if path != "" && template.Default != nil {
				if m, err := readSubagentModel(path); err == nil && buildfeatures.Disabled(m.Provider) {
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
	if buildfeatures.Disabled(m.Provider) {
		return disabledChildError()
	}
	if m.Provider == claudeagent.ProviderID {
		return &subagent.RuntimeError{Code: "override_unavailable", Hint: "/model-sub で Claude を選んでいる間は --model と --effort を使えません"}
	}
	if m.Provider == opencodego.ProviderID {
		return &subagent.RuntimeError{Code: "override_unavailable", Hint: "/model-sub で OpenCode Go を選んでいる間は --model と --effort を使えません。子は /model-sub のモデルを役割ごとの effort で使います。モデルを変えるには /model-sub で選び直してください"}
	}
	if template.Default.Provider != "openai-codex" {
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

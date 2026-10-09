package tui

import (
	"fmt"
	"strings"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
)

// One compact row below the composer. Values change only with provider progress
// or canonical responses; the UI does not estimate tokens from rendered text or
// keep a rate timer running through Operation/approval waits.
func statisticsBar(s Snapshot, u UIState, l layout) line {
	unknown := u.Theme.symbol("—", "-")
	provider, model, effort := unknown, unknown, unknown
	if choice := s.Selection; choice != nil {
		provider = modelcatalog.ProviderName(choice.Provider)
		if choice.Provider == "openai-codex" {
			provider = "Codex"
		}
		if choice.Provider == "claude-code" {
			provider = "Claude"
		}
		if provider == "" {
			provider = unknown
		}
		provider = SafeText(provider)
		if choice.Model != "" {
			model = SafeText(choice.Model)
		}
		if choice.Effort != "" {
			effort = SafeText(string(choice.Effort))
		}
	}
	stats := s.LastGeneration
	if s.Progress != nil && s.Progress.Statistics != nil {
		stats = s.Progress.Statistics
	} else if s.Progress != nil && !s.Progress.Done {
		stats = nil // a new generation has not reported current counters yet
	}
	tokens := llm.TokenCounts{}
	tps := "TPS " + unknown
	if stats != nil && (stats.Model == "" || s.Selection == nil || stats.Model == s.Selection.Model) {
		tokens = stats.Tokens.Safe()
		if value, known := stats.OutputTPS(); known {
			if stats.Source == "provider_api" {
				tps = fmt.Sprintf("TPS %.1f(api)", value)
			} else {
				tps = fmt.Sprintf("TPS~%.1f(est)", value)
			}
		}
	}
	input, output := unknown, unknown
	if tokens.InputKnown {
		input = fmt.Sprint(tokens.Input)
	}
	if tokens.OutputKnown {
		output = fmt.Sprint(tokens.Output)
	}
	usage := "I:" + input + " O:" + output
	if l.width >= 90 {
		usage = "In:" + input + " Out:" + output + "(reported)"
	}
	parts := []string{provider + "/" + model, effort, tps, usage}
	if l.width < 65 {
		parts[0] = middle(model, max(4, l.width-32), u.Theme)
	}
	if l.width < 44 {
		parts = []string{middle(model, max(1, l.width-25), u.Theme), effort, tps}
	}
	if l.width < 25 {
		parts = []string{tps}
	}
	return textLine(strings.Join(parts, " | "), meta).clip(l.width)
}

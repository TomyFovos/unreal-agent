package analysis

import (
	"maps"
	"slices"

	"github.com/unreallabsai/unreal-agent/harness/subagent"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
)

// RoleUsage estimates the size of model input per observed call, independently
// of token totals that the provider may report only for an entire invocation.
type RoleUsage struct {
	Role               string
	Calls              Metric
	InputTokensPerCall Metric
}

func (a *Accumulator) sessionRole() string {
	if a.metrics.layer != "" {
		return viewer.SafeText(a.metrics.layer)
	}
	if a.metrics.parent != "" {
		return subagent.RoleGroup(a.metrics.template)
	}
	return "main"
}

type modelCallUsage struct{ calls, input Metric }

func (m *modelCallUsage) missingCall() {
	m.calls.Unknown++
	m.input.Unknown++
}

func (m *modelCallUsage) add(a *Accumulator, observed bool) {
	if !observed || a.partial {
		m.missingCall()
	}
	for _, request := range a.context.requests {
		if !request.countKnown {
			m.missingCall()
			continue
		}
		m.calls.add(1, "measured")
		if input := request.usage.Input; input != nil && *input >= 0 {
			m.input.add(float64(*input), "measured")
		} else {
			m.input.Unknown++
		}
	}
	for _, turn := range a.context.invocations {
		if turn.latest == nil && turn.single == nil {
			m.missingCall()
		}
	}
}

func (m modelCallUsage) report(role string) RoleUsage {
	if m.calls.Unknown == 0 {
		m.calls.Known = true
	}
	mean := Metric{Unknown: m.input.Unknown}
	if m.input.Known {
		mean.add(m.input.average().Value, "derived")
	}
	return RoleUsage{Role: role, Calls: m.calls, InputTokensPerCall: mean}
}

func (t *Tree) roleUsage(sessions []SessionMetric) []RoleUsage {
	byRole := make(map[string]*modelCallUsage)
	for i, node := range t.nodes {
		role := sessions[i].Layer
		if byRole[role] == nil {
			byRole[role] = &modelCallUsage{}
		}
		byRole[role].add(node.acc, node.observed)
	}
	rows := make([]RoleUsage, 0, len(byRole))
	for _, role := range slices.Sorted(maps.Keys(byRole)) {
		rows = append(rows, byRole[role].report(role))
	}
	return rows
}

package analysis

import (
	"encoding/json/v2"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/subagent"
)

type RoutingCount struct {
	RuleID    string `json:"rule_id"`
	Handled   int    `json:"handled"`
	Forwarded int    `json:"forwarded"`
}

type routingCounts map[string]RoutingCount

func (r routingCounts) add(rule string, handled bool) {
	c := r[rule]
	c.RuleID = rule
	if handled {
		c.Handled++
	} else {
		c.Forwarded++
	}
	r[rule] = c
}

func (r routingCounts) merge(other routingCounts) {
	for rule, value := range other {
		c := r[rule]
		c.RuleID = rule
		c.Handled += value.Handled
		c.Forwarded += value.Forwarded
		r[rule] = c
	}
}

func (r routingCounts) sorted() []RoutingCount {
	var result []RoutingCount
	for _, rule := range slices.Sorted(maps.Keys(r)) {
		result = append(result, r[rule])
	}
	return result
}

func (a *Accumulator) observeRoutingInput(input inbox.Input) {
	if input.Kind == inbox.InputExternal {
		a.routingInputs.add("U1", false)
		return
	}
	peer, err := input.DecodePeerMessage()
	if err != nil {
		return
	}
	switch peer.Kind {
	case "ready", "progress":
		a.routingInputs.add("P1", true)
	case "message":
		switch {
		case peer.RuleID == "Q7" || peer.RuleID == "X1":
			a.routingInputs.add(peer.RuleID, false)
		case strings.HasPrefix(string(input.ID), "approval:"):
			a.routingInputs.add("A3", false)
		}
	case "ended":
		if peer.ReportIncluded {
			a.deliveredReports[operation.ID(peer.Handle)] = true
		}
	}
}

func (a *Accumulator) observeRoutingReport(op operation.Operation) {
	if op.Status != operation.StatusCompleted && op.Status != operation.StatusFailed && op.Status != operation.StatusCanceled {
		return
	}
	if handle, err := subagent.DecodeHandle(op); err == nil && handle.Finish != nil {
		a.deliveredReports[op.ID] = true
	}
}

func (a *Accumulator) routing() routingCounts {
	counts := maps.Clone(a.routingInputs)
	for range a.deliveredReports {
		counts.add("F1", false)
	}
	for _, op := range a.operations {
		if op.ToolName == "SendParent" {
			state, err := operation.DecodeRemoteJobState(op)
			var receipt subagent.ParentMessageReceipt
			if err != nil || state.Plan.Type != subagent.PlanType || json.Unmarshal(state.Handle, &receipt) != nil {
				continue
			}
			switch receipt.RuleID {
			case "Q1", "Q2", "Q3", "Q4", "Q5", "Q6":
				if op.Status == operation.StatusCompleted && receipt.Answer != "" {
					counts.add(receipt.RuleID, true)
				}
			}
			continue
		}
		if a.metrics.parent != "" {
			if rule := sessionstore.AutomaticPermissionRule(op); rule != "" {
				counts.add(rule, true)
			}
		}
	}
	return counts
}

func routingLines(r Report) []string {
	counts := r.Routing
	scope := "session"
	if r.Tree != nil {
		counts, scope = r.Tree.Routing, "session tree"
		if r.Tree.Partial {
			scope += ", partial"
		}
	}
	if len(counts) == 0 {
		return nil
	}
	lines := []string{"Delegation routing · measured counts (" + scope + ")"}
	saved := 0
	for _, c := range counts {
		lines = append(lines, fmt.Sprintf("%s  %d handled · %d forwarded", c.RuleID, c.Handled, c.Forwarded))
		switch c.RuleID {
		case "Q1", "Q2", "Q3", "Q4", "Q5", "Q6":
			saved += c.Handled
		}
	}
	return append(lines, fmt.Sprintf("Estimated parent wakeups saved by brief replies · up to %d", saved))
}

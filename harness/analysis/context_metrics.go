package analysis

import (
	"cmp"
	"slices"
	"strings"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/contextstate"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

type ContextDimensions struct {
	ModelKey
	Strategy string
	Fast     string
}

// ContextNumber exports missing values as null with source "unknown". Missing
// counts additional observations that could not contribute to a known value.
type ContextNumber struct {
	Value   *float64
	Source  string
	Missing int `json:",omitzero"`
}

type ContextNumbers map[string]ContextNumber

type ContextGroup struct {
	ContextDimensions
	Numbers ContextNumbers
}

type ContextSubscription struct {
	ContextDimensions
	Session                    session.ID
	At                         time.Time
	Window, Status             string
	UsedPercent, WindowMinutes ContextNumber
	ResetsAt                   *time.Time
}

type ContextWindow struct {
	OpenedBy string
	At       time.Time
	Groups   []ContextGroup
}

type ContextCompaction struct {
	ContextDimensions
	At              time.Time
	Engine, Outcome string
	Numbers         ContextNumbers
}

type ContextFallback struct {
	ContextDimensions
	At      time.Time
	Reason  string
	Numbers ContextNumbers
}

type ContextReview struct {
	ContextDimensions // the child's model and the strategy at delegation
	ChildStrategy     string
	Numbers           ContextNumbers
}

type ContextScope struct {
	Groups       []ContextGroup
	Questions    []ContextGroup // summed by strategy across runtime selections
	Reviews      []ContextReview
	Subscription []ContextSubscription
}

type ContextSession struct {
	Session session.ID
	Partial bool
	ContextScope
	Windows       []ContextWindow
	Compactions   []ContextCompaction
	Fallbacks     []ContextFallback     `json:",omitempty"`
	PolicyPeriods []ContextPolicyPeriod `json:",omitempty"`
}

type ContextRoutePolicy struct {
	Provider string `json:",omitzero"` // empty is the common policy
	Role     string `json:",omitzero"`
	Policy   sessionstore.WindowPolicy
}

type ContextPolicyPeriod struct {
	Provider    string
	Role        string `json:",omitzero"`
	From, Until *time.Time
	Policy      *sessionstore.WindowPolicy // nil for rebuilds without a recorded policy
	Numbers     ContextNumbers
}

type ContextMeasurements struct {
	PriceTable     string
	Partial        bool
	Totals         ContextScope
	Sessions       []ContextSession
	ActivePolicies []ContextRoutePolicy `json:",omitempty"`
}

type contextCounts map[string]Metric

func (c contextCounts) merge(key string, value Metric) {
	m := c[key]
	m.merge(value)
	c[key] = m
}

func (c contextCounts) add(key string, value float64, source string) {
	m := c[key]
	m.add(value, source)
	c[key] = m
}

func (c contextCounts) unknown(key string) {
	m := c[key]
	m.Unknown++
	c[key] = m
}

func (c contextCounts) tokens(key string, value *int64, source string) {
	if value == nil || *value < 0 {
		c.unknown(key)
		return
	}
	c.add(key, float64(*value), source)
}

func (c contextCounts) zeros(source string, keys ...string) {
	for _, key := range keys {
		if _, exists := c[key]; !exists {
			c.add(key, 0, source)
		}
	}
}

func (c contextCounts) ratio(out, num, den string, scale float64) {
	n, d := c[num], c[den]
	if !n.Known || !d.Known || d.Value <= 0 {
		return
	}
	m := Metric{Unknown: max(n.Unknown, d.Unknown)}
	m.add(scale*n.Value/d.Value, "derived")
	c[out] = m
}

func contextNumber(m Metric) ContextNumber {
	n := ContextNumber{Source: "unknown", Missing: m.Unknown}
	if m.Known {
		n.Value, n.Source = new(m.Value), "measured"
		if m.Derived > 0 {
			n.Source = "derived"
		}
	}
	return n
}

func (c contextCounts) report() ContextNumbers {
	result := make(ContextNumbers, len(c))
	for key, m := range c {
		if !strings.HasPrefix(key, "_") {
			if isContextMean(key) && m.Measured+m.Derived > 1 {
				m = m.average()
				m.Derived++
			}
			result[key] = contextNumber(m)
		}
	}
	return result
}

func isContextMean(key string) bool {
	switch key {
	case "boundary_before_tokens", "boundary_after_tokens", "window_header_tokens", "threshold_tokens", "l2_copy_tokens", "l2_target_tokens",
		"tail_tokens", "tail_items", "recent_events", "recent_event_limit", "excerpt_chars", "boundary_interval_requests",
		"boundary_interval_seconds", "pending_approvals", "pending_user_questions", "pending_child_questions",
		"fit_excerpt_chars", "fit_required_input", "fit_required_tail", "fit_input_budget":
		return true
	}
	return (strings.HasPrefix(key, "Unreal_") || strings.HasPrefix(key, "CLI_")) && !strings.HasSuffix(key, "_compactions")
}

type contextGroups map[ContextDimensions]contextCounts

func (g contextGroups) group(key ContextDimensions) contextCounts {
	if g[key] == nil {
		g[key] = make(contextCounts)
	}
	return g[key]
}

func (g contextGroups) merge(other contextGroups) {
	for key, numbers := range other {
		c := g.group(key)
		for metric, value := range numbers {
			c.merge(metric, value)
		}
	}
}

func compareContextDimensions(a, b ContextDimensions) int {
	if n := cmp.Compare(a.Strategy, b.Strategy); n != 0 {
		return n
	}
	if n := cmp.Compare(modelLabel(a.ModelKey), modelLabel(b.ModelKey)); n != 0 {
		return n
	}
	return cmp.Compare(a.Fast, b.Fast)
}

func (g contextGroups) report(questionsOnly bool) []ContextGroup {
	var rows []ContextGroup
	for key, c := range g {
		c.ratio("within_hit_percent", "_within_paired_read", "_within_paired_input", 100)
		c.ratio("record_previous_tail_percent", "record_previous_tail_calls", "_record_tail_checks", 100)
		c.ratio("questions_per_hour", "user_questions", "work_seconds", 3600)
		numbers := c.report()
		columns := questionColumns
		if !questionsOnly {
			columns = groupColumns(key.Strategy, false)
		}
		for _, col := range columns {
			numbers.ensure(col.key)
		}
		rows = append(rows, ContextGroup{ContextDimensions: key.public(), Numbers: numbers})
	}
	slices.SortFunc(rows, func(a, b ContextGroup) int { return compareContextDimensions(a.ContextDimensions, b.ContextDimensions) })
	return rows
}

func (n ContextNumbers) ensure(key string) {
	if _, exists := n[key]; !exists {
		n[key] = ContextNumber{Source: "unknown"}
	}
}

func (k ContextDimensions) public() ContextDimensions {
	k.Strategy, k.Provider, k.Model = textOrUnknown(k.Strategy), textOrUnknown(k.Provider), textOrUnknown(k.Model)
	k.Effort, k.Fast = textOrUnknown(k.Effort), textOrUnknown(k.Fast)
	return k
}

var windowReasons = []string{"threshold", "over_budget", "header_change", "mid_turn", "selection_change", "provider_overflow", "historical_context"}

var stateCommands = []string{"decide", "plan", "rule", "accept", "reject", "question"}

var machineKinds = []contextstate.EventKind{
	contextstate.SessionChanged, contextstate.RequestChanged, contextstate.RuleRecorded, contextstate.PlanChanged,
	contextstate.DecisionRecorded, contextstate.ChildChanged, contextstate.ApprovalChanged, contextstate.WaitChanged,
	contextstate.QuestionChanged, contextstate.ArtifactChanged, contextstate.IssueChanged, contextstate.RecentChanged,
	contextstate.RecentObserved, contextstate.MetricsChanged, contextstate.BoundaryObserved, contextstate.WindowRebuilt,
}

const historicalSummaryStrategy = "historical summary"

func recordedStrategy(s string) string {
	switch s {
	case contextengine.WindowThreeLayer:
		return s
	case "summary", historicalSummaryStrategy:
		return historicalSummaryStrategy
	default:
		return ""
	}
}

func fastLabel(v *bool) string {
	if v == nil {
		return ""
	}
	if *v {
		return "on"
	}
	return "off"
}

func reportSubscription(id session.ID, key ContextDimensions, at time.Time, u llm.SubscriptionUsage) ContextSubscription {
	r := ContextSubscription{Session: id, ContextDimensions: key.public(), At: at, Window: textOrUnknown(u.Window), Status: textOrUnknown(u.Status),
		UsedPercent: ContextNumber{Source: "unknown"}, WindowMinutes: ContextNumber{Source: "unknown"}}
	if u.UsedPercent != nil {
		r.UsedPercent = ContextNumber{Value: u.UsedPercent, Source: "measured"}
	}
	if u.WindowMinutes != nil {
		r.WindowMinutes = ContextNumber{Value: new(float64(*u.WindowMinutes)), Source: "measured"}
	}
	if !u.ResetsAt.IsZero() {
		r.ResetsAt = &u.ResetsAt
	}
	return r
}

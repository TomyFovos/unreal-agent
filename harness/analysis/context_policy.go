package analysis

import (
	"maps"
	"slices"
	"sort"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

type contextPolicyEvent struct {
	sequence sessionstore.Sequence
	at       time.Time
	provider string
	policy   *sessionstore.ContextPolicies
}

type contextPolicyRollup struct {
	ContextPolicyPeriod
	sequence sessionstore.Sequence
	counts   contextCounts
	cache    CacheShare
}

func newPolicyRollup(provider string, event contextPolicyEvent, policy *sessionstore.WindowPolicy) *contextPolicyRollup {
	p := &contextPolicyRollup{
		ContextPolicyPeriod: ContextPolicyPeriod{Provider: provider, Policy: policy},
		sequence:            event.sequence, counts: make(contextCounts),
	}
	if !event.at.IsZero() {
		p.From = new(event.at)
	}
	return p
}

// Metrics belong to the policy that built the window. A reload can change
// the next rebuild's settings while the preceding projection is still in use.
func (m *contextReplay) policyPeriods(windows map[*contextWindowLog]*windowMeasurements, observed bool, provider string) ([]ContextPolicyPeriod, []string, *contextPolicyEvent) {
	routes := map[string]bool{provider: true}
	rebuilds := make(map[string][]*contextWindowLog)
	for _, w := range m.windows {
		routes[w.key.Provider] = true
		if w.open == "rebuild" {
			rebuilds[w.key.Provider] = append(rebuilds[w.key.Provider], w)
		}
	}
	for _, event := range m.policies {
		routes[event.provider] = true
	}
	if len(rebuilds[""]) == 0 {
		delete(routes, "")
	}

	providers := slices.Sorted(maps.Keys(routes))
	var out []ContextPolicyPeriod
	for _, route := range providers {
		periods := m.routePolicies(route)
		for _, w := range rebuilds[route] {
			i := sort.Search(len(periods), func(i int) bool { return periods[i].sequence > w.sequence })
			if i == 0 {
				unknown := newPolicyRollup(route, contextPolicyEvent{}, nil)
				if len(periods) > 0 {
					unknown.Until = periods[0].From
				}
				periods = append([]*contextPolicyRollup{unknown}, periods...)
				i++
			}
			periods[i-1].rebuild(windows[w])
		}
		for _, p := range periods {
			out = append(out, p.report(observed))
		}
	}
	var latest *contextPolicyEvent
	if len(m.policies) > 0 {
		latest = &m.policies[len(m.policies)-1]
	}
	return out, providers, latest
}

func (m *contextReplay) routePolicies(route string) []*contextPolicyRollup {
	var periods []*contextPolicyRollup
	for _, event := range m.policies {
		policy := event.policy.ForProvider(route)
		if n := len(periods); n > 0 && *periods[n-1].Policy == policy {
			continue
		}
		period := newPolicyRollup(route, event, &policy)
		if n := len(periods); n > 0 {
			periods[n-1].Until = period.From
		}
		periods = append(periods, period)
	}
	return periods
}

func (p *contextPolicyRollup) rebuild(w *windowMeasurements) {
	p.counts.add("rebuilds", 1, "measured")
	var size Metric
	for _, g := range w.groups {
		size.merge(g["boundary_after_tokens"])
		p.counts.merge("record_show_calls", g["record_show_calls"])
		p.counts.merge("record_find_calls", g["record_find_calls"])
	}
	if !size.Known {
		size.Unknown = max(1, size.Unknown)
	}
	p.counts.merge("boundary_after_tokens", size)
	if len(w.requests) == 0 {
		p.cache.missing++
	}
	p.cache.input += w.firstRead.input
	p.cache.read += w.firstRead.read
	p.cache.missing += w.firstRead.missing
}

func (p *contextPolicyRollup) report(observed bool) ContextPolicyPeriod {
	if observed {
		p.counts.zeros("measured", "rebuilds")
		if p.counts["rebuilds"].Value == 0 {
			p.counts.zeros("measured", "record_show_calls", "record_find_calls")
		}
	}
	p.Numbers = p.counts.report()
	read := p.cache.Percent()
	if read.Known {
		read.Derived = 1
	}
	p.Numbers["first_cache_read_share"] = contextNumber(read)
	for _, field := range []string{"rebuilds", "boundary_after_tokens", "record_show_calls", "record_find_calls"} {
		p.Numbers.ensure(field)
	}
	return p.ContextPolicyPeriod
}

func activeContextPolicies(results []contextSessionResult) []ContextRoutePolicy {
	var latest *contextPolicyEvent
	routes := make(map[string]bool)
	for _, r := range results {
		if r.policy != nil && (latest == nil || r.policy.at.After(latest.at)) {
			latest = r.policy
		}
		for _, route := range r.routes {
			if route != "" {
				routes[route] = true
			}
		}
	}
	if latest == nil {
		return nil
	}
	for route := range latest.policy.Routes {
		routes[route] = true
	}
	out := []ContextRoutePolicy{{Policy: latest.policy.Default}}
	for _, provider := range slices.Sorted(maps.Keys(routes)) {
		out = append(out, ContextRoutePolicy{Provider: provider, Policy: latest.policy.ForProvider(provider)})
	}
	return out
}

package analysis

import (
	"cmp"
	"slices"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/contextstate"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func (a *Accumulator) ContextReport(observed bool) *ContextMeasurements {
	return contextReport([]contextSessionResult{a.context.project(observed, nil, a.calls, modelKey(a.active), a.metrics.layer)}, !observed)
}

type windowMeasurements struct {
	groups    contextGroups
	requests  []*contextRequest
	firstRead CacheShare
}

type contextSessionResult struct {
	view     ContextSession
	groups   contextGroups
	strategy contextGroups
	reviews  reviewGroups
	policy   *contextPolicyEvent
	routes   []string
}

func (m *contextReplay) project(observed bool, facts *sessionstore.ContextFacts, calls map[string]*call, fallback ModelKey, role string) contextSessionResult {
	result := contextSessionResult{
		view: ContextSession{Session: m.id, Partial: !observed, Fallbacks: m.fallbacks}, groups: make(contextGroups),
		strategy: make(contextGroups), reviews: make(reviewGroups),
	}
	if len(m.windows) == 0 {
		result.groups.group(ContextDimensions{ModelKey: fallback})
		return result
	}

	windows := make(map[*contextWindowLog]*windowMeasurements, len(m.windows))
	for _, w := range m.windows {
		windows[w] = &windowMeasurements{groups: make(contextGroups)}
	}
	requests := slices.Clone(m.requests)
	cacheByWindow := make(map[*contextWindowLog]sessionstore.WindowCache)
	for _, t := range m.invocations {
		if t.latest == nil && t.single == nil {
			// The CLI can omit per-call observations (and a recorded Turn can
			// be interrupted before any transport evidence is committed).
			requests = append(requests, &contextRequest{contextPosition: contextPosition{after: t.after, window: t.window},
				key: t.key, at: t.at})
		}
		if address, err := t.firstCache.Boundary.Address(); err == nil && address.Session == string(m.id) {
			if w := m.boundaries[sessionstore.Sequence(address.Sequence)]; w != nil {
				cacheByWindow[w] = t.firstCache
			}
		}
	}
	slices.SortStableFunc(requests, func(a, b *contextRequest) int { return cmp.Compare(a.after, b.after) })
	for _, r := range requests {
		windows[r.window].requests = append(windows[r.window].requests, r)
	}
	group := func(p contextPosition) contextCounts {
		w := windows[p.window]
		if p.turn == nil && p.request == nil && len(w.requests) != 0 {
			p.request = w.requests[0]
		}
		return w.groups.group(p.dimensions())
	}

	for _, note := range m.notes {
		group(m.position(note.after)).add("state_"+note.kind, 1, "measured")
	}
	for _, q := range m.questions {
		group(q.contextPosition).add("user_questions", float64(q.count), "measured")
	}
	for _, t := range m.invocations {
		if !t.hasReply {
			windows[t.window].groups.group(t.dimensions()).unknown("user_questions")
		}
	}
	for _, read := range m.reads {
		c := group(m.position(read.After))
		switch read.Action {
		case "show", "find":
			c.add("record_"+read.Action+"_calls", 1, "measured")
			c.tokens("record_"+read.Action+"_bytes", read.Bytes, "measured")
			c.tokens("record_previous_tail_matches", read.PreviousTailHits, "derived")
			if read.PreviousTailHits != nil {
				c.add("record_previous_tail_calls", float64(boolCount(*read.PreviousTailHits > 0)), "derived")
				c.add("_record_tail_checks", 1, "measured")
			} else {
				c.unknown("record_previous_tail_calls")
				c.unknown("_record_tail_checks")
			}
		}
	}

	pending := make(map[*contextWindowLog]*contextstate.PendingCounts)
	if facts != nil {
		for _, fact := range facts.Rows {
			if fact.After > m.cursor {
				break
			}
			p := m.position(fact.After)
			for kind, count := range fact.Events {
				group(p).add("machine_"+string(kind), float64(count), "derived")
			}
			if fact.Pending != nil {
				if w := m.boundaries[fact.After]; w != nil {
					pending[w] = fact.Pending
				}
			}
		}
	}
	result.view.Partial = result.view.Partial || facts == nil || facts.Through < m.cursor
	m.workTime(windows, calls, result.strategy)

	var previous *contextWindowLog
	for _, w := range m.windows {
		log := windows[w]
		if len(log.requests) == 0 && len(log.groups) == 0 && w.open == "initial" && len(m.windows) > 1 {
			continue // a strategy was established before this prelude did work
		}
		key := w.key
		if len(log.requests) > 0 {
			key = log.requests[0].key
			key.Strategy = w.key.Strategy
		}
		if key.ModelKey == (ModelKey{}) {
			key.ModelKey = fallback
		}
		isBoundary := w.open == "rebuild" || w.open == "compaction"
		c := log.groups.group(key)
		if isBoundary {
			c.add("boundaries", 1, "measured")
			if previous != nil && w.key.Strategy == previous.key.Strategy {
				c.merge("boundary_interval_seconds", interval(previous.at, w.at, "derived"))
				count, missing := 0, 0
				for _, r := range windows[previous].requests {
					if r.countKnown {
						count++
					} else {
						missing++
					}
				}
				n := Metric{Unknown: missing}
				if missing == 0 || count > 0 {
					n.add(float64(count), "measured")
				}
				c.merge("boundary_interval_requests", n)
			} else {
				c.unknown("boundary_interval_seconds")
				c.unknown("boundary_interval_requests")
			}
			open := w.pending
			if open == nil && w.open == "compaction" {
				open = pending[w]
			}
			addPending(c, open)
			previous = w
		} else {
			previous = nil
		}
		measureBoundary(c, w, observed)
		for i, r := range log.requests {
			bucket := "within"
			usage := r.usage
			if isBoundary && i == 0 {
				bucket = "first"
				cache := cacheByWindow[w]
				if cache.ReadKnown && usage.Read == nil {
					usage.Read = &cache.Read
				}
				if cache.WriteKnown && usage.Write == nil {
					usage.Write = &cache.Write
				}
				log.firstRead.add(usage.Input, usage.Read)
			}
			k := r.key
			k.Strategy = w.key.Strategy
			measureCache(log.groups.group(k), bucket, r, usage)
		}
		for _, group := range log.groups {
			for _, command := range stateCommands {
				if observed {
					group.zeros("measured", "state_"+command)
				} else {
					group.unknown("state_" + command)
				}
			}
			for _, kind := range machineKinds {
				field := "machine_" + string(kind)
				if facts != nil && facts.Through >= m.cursor && observed {
					group.zeros("derived", field)
				} else {
					group.unknown(field)
				}
			}
			if w.key.Strategy == historicalSummaryStrategy && observed {
				group.zeros("measured", "CLI_compactions", "Unreal_compactions")
			}
			for _, action := range []string{"show", "find"} {
				call, bytes := "record_"+action+"_calls", "record_"+action+"_bytes"
				if w.l3Covered && observed {
					group.zeros("measured", call, bytes)
				} else {
					group.unknown(call)
					group.unknown(bytes)
				}
			}
			for _, field := range []string{"record_previous_tail_calls", "record_previous_tail_matches"} {
				if w.l3Covered && w.boundary != nil && w.boundary.Tail != nil && w.boundary.Tail.Previous != nil && observed {
					group.zeros("derived", field)
				} else {
					group.unknown(field)
				}
			}
		}
	}

	for _, compact := range m.compactions {
		numbers := make(contextCounts)
		for field, value := range compact.numbers {
			numbers[field] = value
		}
		numbers.add("compactions", 1, "measured")
		if compact.outcome == "completed" {
			if requests := windows[compact.window].requests; len(requests) > 0 {
				r := requests[0]
				if r.key.Provider == compact.key.Provider {
					numbers.tokens("post_request_input_tokens", r.usage.Input, "measured")
				}
			}
		}
		for _, field := range compactionFields {
			if _, exists := numbers[field]; !exists {
				numbers.unknown(field)
			}
		}
		result.view.Compactions = append(result.view.Compactions, ContextCompaction{ContextDimensions: compact.key.public(),
			At: compact.at, Engine: compact.engine, Outcome: compact.outcome, Numbers: numbers.report()})
		dest := windows[compact.window].groups.group(compact.key)
		for field, value := range numbers {
			dest.merge(compact.engine+"_"+field, value)
		}
	}
	for _, w := range m.windows {
		log := windows[w]
		if len(log.groups) == 0 {
			continue
		}
		result.groups.merge(log.groups)
		result.view.Windows = append(result.view.Windows, ContextWindow{OpenedBy: w.open, At: w.at, Groups: log.groups.report(false)})
	}
	for key, c := range result.groups {
		result.strategy.group(ContextDimensions{Strategy: key.Strategy}).merge("user_questions", c["user_questions"])
	}
	result.view.Subscription = slices.Clone(m.subscription)
	result.view.PolicyPeriods, result.routes, result.policy = m.policyPeriods(windows, observed, fallback.Provider, role)
	return result
}

func (t *contextTurnLog) dimensions() ContextDimensions {
	key := t.key
	key.Strategy = t.window.key.Strategy
	return key
}

func boolCount(v bool) int {
	if v {
		return 1
	}
	return 0
}

func addPending(c contextCounts, p *contextstate.PendingCounts) {
	if p == nil {
		for _, field := range []string{"pending_user_questions", "pending_child_questions", "pending_approvals"} {
			c.unknown(field)
		}
		return
	}
	c.add("pending_user_questions", float64(p.UserQuestions), "derived")
	c.add("pending_child_questions", float64(p.ChildQuestions), "derived")
	c.add("pending_approvals", float64(p.Approvals), "derived")
}

func measureBoundary(c contextCounts, w *contextWindowLog, observed bool) {
	b := w.boundary
	if w.open != "rebuild" && observed && w.key.Strategy != "" {
		c.zeros("measured", "boundaries")
	}
	if w.open != "rebuild" && observed && w.key.Strategy == contextengine.WindowThreeLayer {
		for _, reason := range windowReasons {
			c.zeros("measured", "reason_"+reason)
		}
	}
	if w.open == "rebuild" {
		for _, reason := range windowReasons {
			c.unknown("reason_" + reason)
		}
		if b != nil && len(b.Causes) > 0 {
			for _, reason := range windowReasons {
				delete(c, "reason_"+reason)
				c.add("reason_"+reason, 0, "measured")
			}
			for _, reason := range b.Causes {
				if reason == "admission" {
					reason = "over_budget"
				}
				// A boundary contributes once to each independently observed cause.
				if slices.Contains(windowReasons, reason) && c["reason_"+reason].Value == 0 {
					c.add("reason_"+reason, 1, "measured")
				}
			}
		}
	}
	if b == nil {
		return
	}
	if b.ThresholdTokens > 0 {
		c.add("threshold_tokens", float64(b.ThresholdTokens), "measured")
	}
	if b.RebuildTargetTokens > 0 {
		c.add("rebuild_target_tokens", float64(b.RebuildTargetTokens), "measured")
	}
	if b.L2TargetTokens > 0 {
		c.add("l2_target_tokens", float64(b.L2TargetTokens), "measured")
	}
	c.add("excerpt_chars", float64(b.ToolOutputExcerptChars), "measured")
	if b.RecentEventLimit != nil {
		c.add("recent_event_limit", float64(*b.RecentEventLimit), "measured")
	}
	if b.Fit == sessionstore.WindowFitExcerpted {
		c.add("fit_excerpted", 1, "measured")
		if b.FitExcerptChars != nil {
			c.add("fit_excerpt_chars", float64(*b.FitExcerptChars), "measured")
		}
		c.tokens("fit_required_input", &b.RequiredInputTokens, "derived")
		c.tokens("fit_required_tail", &b.RequiredTailTokens, "derived")
		c.tokens("fit_input_budget", &b.InputBudgetTokens, "measured")
	}
	if w.open == "rebuild" {
		measureInput(c, "boundary_before_tokens", b.BeforeTokens, b.BeforeMeasurement)
		c.tokens("window_header_tokens", b.HeaderTokens, "derived")
		if b.HeaderTokens != nil {
			var count float64
			if b.MinimumExceedsTarget {
				count = 1
			}
			c.add("required_minimum_over_target", count, "measured")
		} else {
			c.unknown("required_minimum_over_target")
		}
		if b.Estimator != "" {
			c.tokens("boundary_after_tokens", &b.AfterTokens, "derived")
			c.tokens("l2_copy_tokens", &b.L2Tokens, "derived")
			c.add("recent_events", float64(b.RecentEvents), "measured")
		} else {
			c.unknown("boundary_after_tokens")
			c.unknown("l2_copy_tokens")
			c.unknown("recent_events")
		}
		if b.Tail != nil {
			c.tokens("tail_tokens", &b.Tail.Tokens, "derived")
			c.tokens("tail_items", &b.Tail.Items, "measured")
		} else {
			c.unknown("tail_tokens")
			c.unknown("tail_items")
		}
	}
}

func measureCache(c contextCounts, bucket string, request *contextRequest, u llm.RequestUsage) {
	if request.countKnown {
		c.add(bucket+"_requests", 1, "measured")
	} else {
		c.unknown(bucket + "_requests")
	}
	c.tokens(bucket+"_cache_read", u.Read, "measured")
	c.tokens(bucket+"_cache_write", u.Write, "measured")
	inputSource := "measured"
	if u.Uncached != nil { // Claude reports the three components independently.
		inputSource = "derived"
	}
	c.tokens(bucket+"_input", u.Input, inputSource)
	uncachedSource := "measured"
	if u.Uncached == nil && validTokens(u.Input) && validTokens(u.Read) && validTokens(u.Write) && *u.Read <= *u.Input && *u.Write <= *u.Input-*u.Read {
		u.Uncached = new(*u.Input - *u.Read - *u.Write)
		uncachedSource = "derived"
	}
	c.tokens(bucket+"_uncached", u.Uncached, uncachedSource)
	if bucket == "within" {
		if validTokens(u.Input) && validTokens(u.Read) && *u.Input >= *u.Read {
			c.tokens("_within_paired_input", u.Input, "measured")
			c.tokens("_within_paired_read", u.Read, "measured")
		} else {
			c.unknown("_within_paired_input")
			c.unknown("_within_paired_read")
		}
	}

	rates, ok := listPriceRates(request.key.ModelKey, request.at)
	total := 0.0
	all := true
	for _, part := range []struct {
		name   string
		tokens *int64
		rate   float64
	}{
		{"uncached", u.Uncached, rates.input}, {"read", u.Read, rates.read}, {"write", u.Write, rates.write}, {"output", u.Output, rates.output},
	} {
		field := bucket + "_usd_" + part.name
		if !ok || !validTokens(part.tokens) || part.rate == 0 && *part.tokens != 0 {
			c.unknown(field)
			all = false
			continue
		}
		cost := float64(*part.tokens) * part.rate / 1e6
		total += cost
		c.add(field, cost, "derived")
	}
	if all {
		c.add(bucket+"_usd_total", total, "derived")
	} else {
		c.unknown(bucket + "_usd_total")
	}
}

func validTokens(n *int64) bool { return n != nil && *n >= 0 }

type workSpan struct{ start, end time.Time }

func (m *contextReplay) workTime(windows map[*contextWindowLog]*windowMeasurements, calls map[string]*call, strategy contextGroups) {
	type workGroup struct {
		window *contextWindowLog
		key    ContextDimensions
	}
	groups := make(map[workGroup][]workSpan)
	byStrategy := make(map[string][]workSpan)
	add := func(t *contextTurnLog, start, end time.Time) {
		key := t.dimensions()
		group := workGroup{t.window, key}
		span := workSpan{start, end}
		groups[group] = append(groups[group], span)
		byStrategy[key.Strategy] = append(byStrategy[key.Strategy], span)
	}
	for _, t := range m.invocations {
		add(t, t.at, t.end)
	}
	for _, call := range calls {
		if t := m.turns[call.turn]; t != nil {
			add(t, call.start, call.end)
		}
	}
	for group, spans := range groups {
		windows[group.window].groups.group(group.key).merge("work_seconds", workSeconds(spans))
	}
	for key, spans := range byStrategy {
		strategy.group(ContextDimensions{Strategy: key}).merge("work_seconds", workSeconds(spans))
	}
}

func workSeconds(spans []workSpan) Metric {
	var result Metric
	slices.SortFunc(spans, func(a, b workSpan) int { return a.start.Compare(b.start) })
	var through time.Time
	for _, span := range spans {
		if span.start.IsZero() || span.end.IsZero() || span.end.Before(span.start) {
			result.Unknown++
			continue
		}
		start := span.start
		if through.After(start) {
			start = through
		}
		if !span.end.Before(start) {
			result.add(span.end.Sub(start).Seconds(), "derived")
			through = span.end
		}
	}
	return result
}

type reviewKey struct {
	ContextDimensions
	childStrategy string
}
type reviewGroups map[reviewKey]contextCounts

func (g reviewGroups) child(key reviewKey, child SessionMetric, parentObserved bool) {
	c := make(contextCounts)
	switch child.Finish {
	case "completed":
		c.add("normal", 1, "measured")
		switch child.Review {
		case "accept":
			c.add("accepted", 1, "measured")
			c.add("reviewed", 1, "measured")
		case "reject":
			c.add("rejected", 1, "measured")
			c.add("reviewed", 1, "measured")
		default:
			if parentObserved {
				c.add("unreviewed", 1, "measured")
			} else {
				c.unknown("unreviewed")
				c.unknown("reviewed")
				c.unknown("accepted")
				c.unknown("rejected")
			}
		}
	case "failed":
		c.add("failed", 1, "measured")
	case "canceled":
		c.add("canceled", 1, "measured")
	default:
		c.unknown("normal")
		c.unknown("reviewed")
		c.unknown("unreviewed")
		c.unknown("failed")
		c.unknown("canceled")
		c.unknown("accepted")
		c.unknown("rejected")
	}
	c.zeros("measured", "normal", "reviewed", "accepted", "rejected", "unreviewed", "failed", "canceled")
	if g[key] == nil {
		g[key] = make(contextCounts)
	}
	for field, value := range c {
		g[key].merge(field, value)
	}
}

func (g reviewGroups) merge(other reviewGroups) {
	for key, numbers := range other {
		if g[key] == nil {
			g[key] = make(contextCounts)
		}
		for field, value := range numbers {
			g[key].merge(field, value)
		}
	}
}

func (g reviewGroups) report() []ContextReview {
	var rows []ContextReview
	for key, counts := range g {
		counts.ratio("rejected_percent", "rejected", "reviewed", 100)
		if _, ok := counts["rejected_percent"]; !ok {
			counts.unknown("rejected_percent")
		}
		rows = append(rows, ContextReview{ContextDimensions: key.ContextDimensions.public(), ChildStrategy: textOrUnknown(key.childStrategy), Numbers: counts.report()})
	}
	slices.SortFunc(rows, func(a, b ContextReview) int {
		if n := compareContextDimensions(a.ContextDimensions, b.ContextDimensions); n != 0 {
			return n
		}
		return cmp.Compare(a.ChildStrategy, b.ChildStrategy)
	})
	return rows
}

func contextReport(results []contextSessionResult, partial bool) *ContextMeasurements {
	report := &ContextMeasurements{PriceTable: PriceTableSource, Partial: partial}
	report.ActivePolicies = activeContextPolicies(results)
	groups, strategy, reviews := make(contextGroups), make(contextGroups), make(reviewGroups)
	for _, r := range results {
		groups.merge(r.groups)
		strategy.merge(r.strategy)
		reviews.merge(r.reviews)
		r.view.Groups, r.view.Questions, r.view.Reviews = r.groups.report(false), r.strategy.report(true), r.reviews.report()
		report.Sessions = append(report.Sessions, r.view)
		report.Totals.Subscription = append(report.Totals.Subscription, r.view.Subscription...)
		report.Partial = report.Partial || r.view.Partial
	}
	report.Totals.Groups, report.Totals.Questions, report.Totals.Reviews = groups.report(false), strategy.report(true), reviews.report()
	return report
}

func (t *Tree) contextReport(root *Accumulator, sessions []SessionMetric) *ContextMeasurements {
	results := make([]contextSessionResult, 0, len(t.nodes))
	accumulators := make(map[session.ID]*Accumulator, len(t.nodes))
	index := make(map[session.ID]int, len(t.nodes))
	for i, n := range t.nodes {
		a := n.acc
		if root != nil && root.id == a.id && root.context.cursor >= a.context.cursor {
			a = root
		}
		accumulators[a.id], index[a.id] = a, i
		results = append(results, a.context.project(n.observed, n.facts, a.calls, sessions[i].ModelKey, sessions[i].Layer))
	}
	for _, child := range sessions {
		parent := accumulators[child.ParentID]
		if parent == nil {
			continue
		}
		var key reviewKey
		key.ModelKey = child.ModelKey
		if start, ok := parent.context.childStarts[string(child.Handle)]; ok {
			key.Strategy = parent.context.position(start).dimensions().Strategy
		}
		a := accumulators[child.ID]
		for _, w := range a.context.windows {
			if !w.hasTurn {
				continue
			}
			if key.childStrategy == "" && w.key.Strategy != "" {
				key.childStrategy = w.key.Strategy
			} else if w.key.Strategy != key.childStrategy {
				key.childStrategy = "mixed"
			}
		}
		for _, request := range child.Requests {
			if request.ModelKey == child.ModelKey && request.Kind == "response" {
				key.Fast = request.Fast
				break
			}
		}
		p := index[child.ParentID]
		results[p].reviews.child(key, child, t.nodes[p].observed)
	}
	return contextReport(results, t.partial)
}

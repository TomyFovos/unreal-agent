package analysis

import (
	"cmp"
	"slices"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
)

// Dashboard carries presentation inputs derived from the same canonical
// prefix as Report. Child titles come from task briefs and stay out of export.
type Dashboard struct {
	Usage                       []UsageSample
	Children                    []ChildSignal
	Tools                       []ToolSample
	Failures                    []Event
	ProviderOK                  []time.Time
	Context                     ContextSignal
	BoundaryWrites, OtherWrites Metric
	Wall                        WallSignal
	State                       string
	Approval                    bool
}

type UsageSample struct {
	Session session.ID
	Role    string
	ModelKey
	At    time.Time
	Usage viewer.Usage
}

type ChildSignal struct {
	ID                         session.ID
	Title, Role, State, Result string
	ModelKey
	Start, End, LastActivity time.Time
}

type ToolSample struct {
	Name, Result string
	Start, End   time.Time
	ChildWait    bool
}

func (t ToolSample) Duration() Metric { return interval(t.Start, t.End, "derived") }

type Sample struct {
	At    time.Time
	Value float64
	Known bool
}

type BoundarySignal struct {
	At     time.Time
	Causes []string
}

type ContextSignal struct {
	Requests                 []Sample
	Boundaries               []BoundarySignal
	Threshold, L1, L2        Metric
	FirstRead, SteadyRead    Metric
	FirstWrites, OtherWrites Metric
	Legacy                   int
}

func (a *Accumulator) dashboard(r *Report, tree *Tree, rows []viewer.Row) *Dashboard {
	d := &Dashboard{Context: a.context.dashboard(a.pruning.Epochs)}
	d.BoundaryWrites, d.OtherWrites = d.Context.FirstWrites, d.Context.OtherWrites
	rootTools := a.toolSamples()
	d.Wall = a.wallSignal(rootTools)
	if tree == nil {
		role := a.sessionRole()
		var calls modelCallUsage
		calls.add(a, !r.Partial)
		r.RoleUsage = []RoleUsage{calls.report(role)}
		d.add(a, role, rootTools)
		for _, row := range rows {
			if row.ParentID != a.id {
				continue
			}
			child := ChildSignal{ID: row.ID, Title: row.Label, ModelKey: modelKey(row.Selection), Start: row.CreatedAt, LastActivity: row.LastActivityAt}

			if row.ObservedModel != "" {
				child.Model = row.ObservedModel
			}
			if row.Finish != nil {
				child.Result, child.End = row.Finish.Status, row.Finish.RecordedAt
			}
			if row.End != nil && child.End.IsZero() {
				child.End = row.End.EndedAt
			}
			child.State, child.Result = childState(child.Result, row.ParentOperationStatus, row.Runtime, child.End)
			d.addChild(child)
			if row.Usage.Known {
				d.Usage = append(d.Usage, UsageSample{Session: row.ID, Role: child.Role, ModelKey: child.ModelKey, Usage: row.Usage})
			}
		}
		d.resolveUsageProviders()
		return d
	}
	r.RoleUsage = tree.roleUsage(r.Tree.Sessions)

	live := make(map[session.ID]viewer.Row, len(rows))
	for _, row := range rows {
		live[row.ID] = row
	}
	for i, n := range tree.nodes {
		s := r.Tree.Sessions[i]
		if i == 0 {
			d.add(a, s.Layer, rootTools)
			continue
		}
		d.add(n.acc, s.Layer, n.acc.toolSamples())
		context := n.acc.context.dashboard(0)
		d.BoundaryWrites.merge(context.FirstWrites)
		d.OtherWrites.merge(context.OtherWrites)
		row := live[s.ID]
		child := ChildSignal{ID: s.ID, Title: s.title, Role: s.Layer, ModelKey: s.ModelKey,
			Start: s.StartedAt, End: s.EndedAt, LastActivity: n.acc.lastActivity}
		if row.LastActivityAt.After(child.LastActivity) {
			child.LastActivity = row.LastActivityAt
		}
		child.State, child.Result = childState(s.Finish, n.status, row.Runtime, child.End)
		d.addChild(child)
	}
	d.resolveUsageProviders()
	return d
}

// Older responses may omit the provider. Attribute those samples when the
// same model has exactly one recorded provider in this tree.
func (d *Dashboard) resolveUsageProviders() {
	providers := make(map[string]string)
	ambiguous := make(map[string]bool)
	for _, s := range d.Usage {
		if s.Provider == "" || s.Model == "" {
			continue
		}
		if prior := providers[s.Model]; prior != "" && prior != s.Provider {
			ambiguous[s.Model] = true
		}
		providers[s.Model] = s.Provider
	}
	for i, s := range d.Usage {
		if s.Provider == "" && !ambiguous[s.Model] {
			d.Usage[i].Provider = providers[s.Model]
		}
	}
}

func childState(result string, status operation.Status, live viewer.Liveness, end time.Time) (string, string) {
	switch {
	case result == "failure" || result == "failed" || status == operation.StatusFailed:
		return "ended", "failed"
	case result == "canceled" || status == operation.StatusCanceled:
		return "ended", "canceled"
	case result == "success" || result == "completed" || status == operation.StatusCompleted:
		return "ended", "completed"
	case !end.IsZero() || live == viewer.RuntimeStopped:
		return "stopped", result
	case live == viewer.RuntimeRunning || status == operation.StatusReady || status == operation.StatusAwaiting || status == operation.StatusCanceling:
		return "running", result
	default:
		return "", result
	}
}

func (d *Dashboard) addChild(child ChildSignal) {
	d.Children = append(d.Children, child)
	if child.Result == "failed" {
		d.Failures = append(d.Failures, Event{At: child.End, Kind: "child", Name: child.Title})
	}
	// Child execution can overlap main work. A child lifetime alone provides
	// no evidence that the main Session was waiting for it.
	if child.End.After(child.Start) {
		d.Wall.add(TimeUnattributed, child.Start, child.End)
	} else if child.State == "running" {
		d.Wall.add(TimeUnattributed, child.Start, time.Time{})
	}
}

func (d *Dashboard) add(a *Accumulator, role string, tools []ToolSample) {
	d.Usage = append(d.Usage, a.usageSamples(role)...)
	d.Failures = append(d.Failures, a.errors...)
	d.ProviderOK = append(d.ProviderOK, a.providerOK...)
	for _, tool := range tools {
		d.Tools = append(d.Tools, tool)
		if tool.Result == "failed" {
			d.Failures = append(d.Failures, Event{At: tool.End, Kind: "tool", Name: tool.Name})
		}
	}
}

func (a *Accumulator) usageSamples(role string) []UsageSample {
	var rows []UsageSample
	execution := func(id session.TurnID) session.TurnID {
		if origin := a.continuations[id]; origin != "" {
			return origin
		}
		return id
	}
	type usageGroup struct{ totals, split []llm.ActivityRecord }
	groups := make(map[session.TurnID]*usageGroup)
	var order []session.TurnID
	for _, activity := range a.activities {
		if activity.Usage == nil || activity.Kind != "provider_usage" && activity.Kind != "claude_usage" {
			continue
		}
		if activity.ID == "" {
			if activity.Kind == "claude_usage" {
				rows = append(rows, a.usageSample(role, activity))
			}
			continue
		}
		id := execution(session.TurnID(activity.ID))
		group := groups[id]
		if group == nil {
			group = &usageGroup{}
			groups[id] = group
			order = append(order, id)
		}
		if activity.Kind == "claude_usage" {
			group.totals = append(group.totals, activity)
		} else {
			group.split = append(group.split, activity)
		}
	}
	covered := make(map[session.TurnID]bool)
	for _, id := range order {
		group := groups[id]
		selected := group.totals
		// The CLI's execution total supersedes interim responses, including
		// tool continuations. A breakdown can replace it only as a whole:
		// dropping just a bad legacy row would retain its inflated siblings.
		if reconciledUsage(group.split, group.totals) {
			selected = group.split
		}
		for _, activity := range selected {
			rows = append(rows, a.usageSample(role, activity))
		}
		covered[id] = len(selected) != 0
	}
	finalResponse := make(map[session.TurnID]bool)
	for _, req := range a.metrics.requests {
		if req.Kind == "response" && !req.toolBatch {
			finalResponse[execution(req.turn)] = true
		}
	}
	for _, req := range a.metrics.requests {
		if req.Kind == "response" {
			id := execution(req.turn)
			if covered[id] {
				continue
			}
			if req.toolBatch && !finalResponse[id] {
				// Only an interim tool batch survives for this execution.
				// Mark the missing total without mixing in a partial counter.
				req.Usage = viewer.Usage{Partial: true}
				covered[id] = true
			}
		}
		rows = append(rows, UsageSample{Session: a.id, Role: role, ModelKey: req.ModelKey, At: req.At, Usage: req.Usage})
	}
	return rows
}

func (a *Accumulator) usageSample(role string, activity llm.ActivityRecord) UsageSample {
	var usage viewer.Usage
	viewer.AddUsage(&usage, *activity.Usage)
	return UsageSample{Session: a.id, Role: role, ModelKey: activityModel(activity), At: activity.EndedAt, Usage: usage}
}

func reconciledUsage(split, totals []llm.ActivityRecord) bool {
	if len(split) == 0 || len(totals) > 1 {
		return false
	}
	type counter struct {
		field llm.UsageField
		value func(*llm.Usage) int64
	}
	counters := []counter{
		{llm.UsageInput, func(u *llm.Usage) int64 { return u.InputTokens }},
		{llm.UsageCachedInput, func(u *llm.Usage) int64 { return u.CachedInputTokens }},
		{llm.UsageCacheWriteInput, func(u *llm.Usage) int64 { return u.CacheWriteInputTokens }},
		{llm.UsageOutput, func(u *llm.Usage) int64 { return u.OutputTokens }},
		{llm.UsageReasoning, func(u *llm.Usage) int64 { return u.ReasoningTokens }},
	}
	for _, r := range split {
		if r.Model == "" || r.Usage.CachedInputTokens > r.Usage.InputTokens && !slices.Contains(r.Usage.Unknown, llm.UsageInput) {
			return false
		}
	}
	for _, c := range counters {
		var remaining int64
		match := len(totals) == 1 && !slices.Contains(totals[0].Usage.Unknown, c.field)
		if match {
			remaining = c.value(totals[0].Usage)
		}
		for _, r := range split {
			value := c.value(r.Usage)
			if value < 0 || match && (value > remaining || slices.Contains(r.Usage.Unknown, c.field)) {
				return false
			}
			if match {
				remaining -= value
			}
		}
		if match && remaining != 0 {
			return false
		}
	}
	return true
}

func activityModel(a llm.ActivityRecord) ModelKey {
	return ModelKey{viewer.SafeText(a.Provider), viewer.SafeText(a.Model), viewer.SafeText(a.Effort)}
}

func (a *Accumulator) toolSamples() []ToolSample {
	native := make(map[string]ToolSample)
	for _, activity := range a.activities {
		if activity.Kind != "native_bash" && activity.Kind != "native_tool" {
			continue
		}
		_, exists := native[activity.ID]
		if !exists || !activity.EndedAt.IsZero() {
			name := activity.Tool
			if name == "" && activity.Kind == "native_bash" {
				name = "Bash"
			}
			native[activity.ID] = ToolSample{Name: name, Start: activity.StartedAt, End: activity.EndedAt}
		}
	}
	out := make([]ToolSample, 0, len(a.calls))
	for _, call := range a.calls {
		sample, ok := native[call.id]
		if !ok {
			sample = ToolSample{Start: call.start, End: call.end}
		}
		if sample.Name == "" {
			sample.Name = call.name
		}
		sample.Result = terminalCall(call, a.operations)
		sample.ChildWait = call.childWait
		if call.status != nil && !sample.ChildWait {
			for _, id := range call.status.Status.WaitingFor {
				if operationWaitsForChildren(a.operations[id]) {
					sample.ChildWait = true
					break
				}
			}
		}
		out = append(out, sample)
		delete(native, call.id)
	}
	for _, sample := range native {
		out = append(out, sample)
	}
	for i := range out {
		if out[i].Result == "" && !out[i].End.IsZero() {
			out[i].Result = "unknown"
		}
	}
	slices.SortFunc(out, func(a, b ToolSample) int {
		return cmp.Or(a.Start.Compare(b.Start), cmp.Compare(a.Name, b.Name), a.End.Compare(b.End), cmp.Compare(a.Result, b.Result))
	})
	return out
}

func (m *contextReplay) dashboard(prunes int) ContextSignal {
	s := ContextSignal{Legacy: len(m.compactions) + prunes}
	if len(m.windows) == 0 {
		return s
	}
	var first, steady CacheShare
	var latest *sessionstore.WindowBoundary
	firstRequest := make(map[*contextWindowLog]bool)
	cache := make(map[*contextWindowLog]sessionstore.WindowCache)
	for _, t := range m.invocations {
		if address, err := t.firstCache.Boundary.Address(); err == nil && address.Session == string(m.id) {
			cache[m.boundaries[sessionstore.Sequence(address.Sequence)]] = t.firstCache
		}
	}
	for _, w := range m.windows {
		if w.open == "window start" || w.open == "rebuild" {
			latest = w.boundary
		}
		if w.open == "rebuild" {
			b := BoundarySignal{At: w.at}
			if w.boundary != nil {
				b.Causes = w.boundary.Causes
			}
			s.Boundaries = append(s.Boundaries, b)
		}
	}
	requests := slices.Clone(m.requests)
	slices.SortStableFunc(requests, func(a, b *contextRequest) int { return cmp.Compare(a.after, b.after) })
	for _, r := range requests {
		u := r.usage
		sample := Sample{At: r.at}
		if validTokens(u.Input) {
			sample.Value, sample.Known = float64(*u.Input), true
		}
		s.Requests = append(s.Requests, sample)
		if r.window.key.Strategy != contextengine.WindowThreeLayer {
			continue
		}
		isFirst := r.window.open == "rebuild" && !firstRequest[r.window]
		firstRequest[r.window] = true
		if isFirst {
			if c := cache[r.window]; u.Read == nil && c.ReadKnown {
				u.Read = &c.Read
			}
			if c := cache[r.window]; u.Write == nil && c.WriteKnown {
				u.Write = &c.Write
			}
			first.add(u.Input, u.Read)
			s.FirstWrites.addTokens(u.Write)
		} else {
			steady.add(u.Input, u.Read)
			s.OtherWrites.addTokens(u.Write)
		}
	}
	s.FirstRead, s.SteadyRead = first.Percent(), steady.Percent()
	if latest != nil {
		if latest.ThresholdTokens > 0 {
			s.Threshold.add(float64(latest.ThresholdTokens), "measured")
		}
		if latest.Tail != nil {
			s.L1.addTokens(&latest.Tail.Tokens)
		}
		if latest.Estimator != "" {
			s.L2.addTokens(&latest.L2Tokens)
		}
	}
	return s
}

func (m *Metric) addTokens(tokens *int64) {
	if validTokens(tokens) {
		m.add(float64(*tokens), "measured")
	} else {
		m.Unknown++
	}
}

type CacheShare struct {
	input, read float64
	missing     int
}

func (c *CacheShare) add(input, read *int64) {
	if !validTokens(input) || !validTokens(read) || *read > *input {
		c.missing++
		return
	}
	c.input += float64(*input)
	c.read += float64(*read)
}

func (c CacheShare) Percent() Metric {
	return Metric{Known: c.input > 0, Value: percent(c.read, c.input), Unknown: c.missing}
}

func percent(value, total float64) float64 {
	if total <= 0 {
		return 0
	}
	return 100 * value / total
}

func knownToken(u viewer.Usage, field llm.UsageField, n int64) *int64 {
	if !u.Known || n < 0 || slices.Contains(u.Unknown, field) {
		return nil
	}
	return new(n)
}

// TokenMix keeps unknown fields independent. Weighted is a display ranking,
// with one unit for each uncached input, cache write or output token.
type TokenMix struct {
	Read, Write, Uncached, UnsplitInput, Output, Weighted, Input Metric
	Cache                                                        CacheShare
}

func (t *TokenMix) Add(u viewer.Usage) {
	in := knownToken(u, llm.UsageInput, u.Input)
	read := knownToken(u, llm.UsageCachedInput, u.CachedInput)
	write := knownToken(u, llm.UsageCacheWriteInput, u.CacheWriteInput)
	out := knownToken(u, llm.UsageOutput, u.Output)
	t.Input.addTokens(in)
	t.Read.addTokens(read)
	t.Write.addTokens(write)
	t.Output.addTokens(out)
	t.Cache.add(in, read)
	if in != nil && read != nil && *in >= *read {
		if write != nil && *in-*read >= *write {
			t.Uncached.addTokens(new(*in - *read - *write))
		} else {
			t.Uncached.Unknown++
			if write == nil {
				t.UnsplitInput.addTokens(new(*in - *read))
			}
		}
		if out != nil {
			t.Weighted.add(float64(*in-*read)+float64(*out), "derived")
			return
		}
	} else {
		t.Uncached.Unknown++
	}
	t.Weighted.Unknown++
}

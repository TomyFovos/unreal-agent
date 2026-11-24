package analysis

import (
	"context"
	"crypto/sha256"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/subagent"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
)

type InspectFunc func(context.Context, session.ID, sessionstore.Sequence, int) (host.View, error)

type SessionMetric struct {
	ModelKey
	ID, ParentID                            session.ID
	Handle                                  operation.ID
	Layer, LayerSource, SelectionSource     string
	Observed                                bool
	Finish                                  string
	StartedAt, EndedAt                      time.Time
	WallTime, FirstResponse, UsageLimitWait Metric
	UsageRetryAt                            time.Time `json:",omitzero"`
	Requests                                []RequestMetric
	Review                                  string
	RejectionEvents                         int
	Redelegated                             bool   // derived from a later start with the same parent/title
	Task                                    string // derived opaque task identity; never exports instructions
	title                                   string
}

type LayerModel struct {
	ModelKey
	Layer                                                                                  string
	Fast                                                                                   string `json:",omitzero"`
	Sessions, Unobserved, RecordedLayers, DerivedLayers                                    int
	Requests                                                                               int
	Usage                                                                                  viewer.Usage
	OutputTPS, RequestDuration, ChildWallTime, FirstResponse, UsageLimitWait, ListPriceUSD Metric
	timedOutput                                                                            int64
	generationSeconds                                                                      float64
}

type Rework struct {
	Normal, Reviewed, Accepted, Rejected, Unreviewed int
	Reworked, Redelegated, RejectionEvents           int
	Failed, Canceled, UnknownEnd                     int
	// Both denominators are Reviewed: normally ended workers with an explicit
	// assessment. ReworkRate also includes the separately labelled inference.
	RejectRate, ReworkRate Metric
}

type TaskMetric struct {
	ModelKey                       // initial worker model; all later attempts stay with this task
	ID                             string
	ParentID                       session.ID
	Attempts                       int
	MixedModels                    bool
	Children                       []session.ID
	WallTime, Elapsed, TotalTokens Metric
	Usage                          viewer.Usage
}

type WorkerSummary struct {
	ModelKey
	Rework                                            Rework
	Tasks, Attempts, MixedModelTasks                  int
	WallTime, TimePerTask, TotalTokens, TokensPerTask Metric
	Elapsed, ElapsedPerTask                           Metric
	Usage                                             viewer.Usage
}

type TreeReport struct {
	Version             int
	ObservedAt          time.Time
	Partial             bool
	PriceTable          string
	Layers              []LayerModel
	Workers             []WorkerSummary
	Sessions            []SessionMetric
	Tasks               []TaskMetric
	Routing             []RoutingCount       `json:",omitzero"`
	ContextMeasurements *ContextMeasurements `json:",omitzero"`
}

type treeNode struct {
	acc             *Accumulator
	after           sessionstore.Sequence
	parent          session.ID
	handle          operation.ID
	template, title string
	layer           string
	selection       *sessionstore.RuntimeSelection
	status          operation.Status
	observed        bool
	facts           *sessionstore.ContextFacts
	operations      []operation.Operation
	archivedEnd     time.Time
}
type Tree struct {
	nodes      []*treeNode
	observedAt time.Time
	partial    bool
}

func (t *Tree) SubscriptionObservations() llm.SubscriptionObservations {
	var latest llm.SubscriptionObservations
	for _, n := range t.nodes {
		latest.Merge(n.acc.subscriptionUsage)
	}
	return latest.Clone()
}

// ReadTree traverses canonical start Operations, never the live-session list:
// the same reader therefore includes archived children and grandchildren.
// Missing descendants remain explicit unobserved nodes, not measured zeros.
func ReadTree(ctx context.Context, root session.ID, inspect InspectFunc) (*Tree, error) {
	tree := &Tree{}
	if err := tree.Refresh(ctx, root, inspect); err != nil {
		return nil, err
	}
	return tree, nil
}

// Refresh reuses canonical prefixes, including those of archived descendants.
// Callers serialize Refresh and report projection on this tree.
func (t *Tree) Refresh(ctx context.Context, root session.ID, inspect InspectFunc) error {
	cached := make(map[session.ID]*treeNode, len(t.nodes))
	if len(t.nodes) > 0 && t.nodes[0].acc.id == root {
		for _, n := range t.nodes {
			cached[n.acc.id] = n
		}
	}
	reuse := func(id session.ID) *treeNode {
		if n := cached[id]; n != nil {
			return n
		}
		return &treeNode{acc: New(id)}
	}
	queue := []*treeNode{reuse(root)}
	seen := map[session.ID]bool{root: true}
	var nodes []*treeNode
	partial := false
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		err := n.read(ctx, inspect)
		n.observed = err == nil
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if len(nodes) == 0 {
				return err
			}
			partial = true
		}
		nodes = append(nodes, n)
		for _, op := range n.operations {
			p, err := subagent.DecodePlan(op)
			if err != nil {
				if state, e := operation.DecodeRemoteJobState(op); e == nil && state.Plan.Type == subagent.PlanType {
					partial = true
				}
				continue
			}
			if p.Action != "start" || p.ParentID != n.acc.id {
				continue
			}
			if seen[p.ChildID] {
				partial = true
				continue
			}
			seen[p.ChildID] = true
			child := reuse(p.ChildID)
			child.parent, child.handle, child.status = n.acc.id, op.ID, op.Status
			child.template, child.layer, child.title = p.Template, p.Configuration.Layer, taskTitle(p.Title())
			child.selection = sessionstore.SelectionFromConfiguration(p.Configuration.Runtime)
			queue = append(queue, child)
		}
	}
	t.nodes, t.partial, t.observedAt = nodes, partial, time.Now().UTC()
	return nil
}

func (n *treeNode) read(ctx context.Context, inspect InspectFunc) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		v, err := inspect(ctx, n.acc.id, n.after, 256)
		if err != nil {
			return err
		}
		page := v.History
		if id := v.Session.Session.ID; id != "" && id != n.acc.id {
			return viewer.ErrInvalidPage
		}
		if page.NextAfter != n.after+sessionstore.Sequence(len(page.Items)) || page.More && len(page.Items) == 0 {
			return viewer.ErrInvalidPage
		}
		for i, item := range page.Items {
			if item.Sequence != n.after+sessionstore.Sequence(i)+1 {
				return viewer.ErrInvalidPage
			}
		}
		n.acc.Created(v.Session.Session.CreatedAt)
		for _, item := range page.Items {
			n.acc.Apply(item)
		}
		if facts := v.ContextFacts; facts != nil && facts.Through == page.NextAfter {
			if n.after == 0 {
				n.facts = &sessionstore.ContextFacts{}
			}
			if n.facts != nil && n.facts.Through == n.after {
				n.facts.Through = page.NextAfter
				n.facts.Rows = append(n.facts.Rows, facts.Rows...)
			}
		}
		n.after = page.NextAfter
		n.operations = slices.Clone(v.Operations)
		sort.Slice(n.operations, func(i, j int) bool { return n.operations[i].ID < n.operations[j].ID })
		for _, op := range n.operations {
			n.acc.operations[op.ID] = op
		}
		if !page.More {
			n.archivedEnd = v.ArchivedEndedAt
			return nil
		}
	}
}

// Report projects the same root prefix as the tree. Canonical child usage and
// results, including archived children, need no separate transcript reads.
func (t *Tree) Report(now time.Time, rows []viewer.Row) Report {
	root := t.nodes[0]
	tree := t.Snapshot(nil)
	live := make(map[session.ID]viewer.Row, len(rows))
	for _, row := range rows {
		live[row.ID] = row
	}
	direct := make([]viewer.Row, 0, len(tree.Sessions))
	for i, s := range tree.Sessions {
		n := t.nodes[i]
		if n.parent != root.acc.id && n != root {
			continue
		}
		row := viewer.Row{ID: s.ID, ParentID: n.parent, ParentOperationStatus: n.status, Cursor: n.after,
			More: !n.observed, Runtime: live[s.ID].Runtime, Usage: n.acc.usage, Selection: n.acc.active}
		if row.Runtime == "" {
			row.Runtime = viewer.RuntimeUnknown
		}
		if s.Finish != "" {
			row.Finish = &viewer.Finish{Status: s.Finish, RecordedAt: s.EndedAt}
			row.Runtime = viewer.RuntimeStopped
		}
		end := s.EndedAt
		if end.IsZero() && row.Runtime == viewer.RuntimeRunning {
			end = now
		}
		if !s.StartedAt.IsZero() && !end.IsZero() && !end.Before(s.StartedAt) {
			row.Elapsed = viewer.Duration{Known: true, Value: end.Sub(s.StartedAt)}
		}
		if len(n.acc.turns) > 0 {
			row.ObservedModel = n.acc.turns[len(n.acc.turns)-1].ObservedModel
		}
		row.Operations = make([]viewer.OperationRow, len(n.operations))
		direct = append(direct, row)
	}
	r := root.acc.Snapshot(now, false, false, direct, nil)
	r.Tree = tree
	r.Dashboard = root.acc.dashboard(&r, t, rows)
	return r
}

func taskTitle(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "#")); line != "" {
			return strings.Join(strings.Fields(line), " ")
		}
	}
	return ""
}

func interval(start, end time.Time, source string) Metric {
	m := Metric{}
	if start.IsZero() || end.IsZero() || end.Before(start) {
		m.Unknown++
		return m
	}
	m.add(end.Sub(start).Seconds(), source)
	return m
}

func (t *Tree) Snapshot(root *Accumulator) *TreeReport {
	r := &TreeReport{Version: 1, ObservedAt: t.observedAt, Partial: t.partial, PriceTable: PriceTableSource}
	metrics := map[session.ID]*sessionMetrics{}
	routing := routingCounts{}
	for _, n := range t.nodes {
		metrics[n.acc.id] = n.acc.metrics
		if root != nil && root.id == n.acc.id {
			routing.merge(root.routing())
		} else {
			routing.merge(n.acc.routing())
		}
	}
	if root != nil {
		metrics[root.id] = root.metrics
	}
	for _, n := range t.nodes {
		m := metrics[n.acc.id]
		s := SessionMetric{ID: n.acc.id, ParentID: n.parent, Handle: n.handle, ModelKey: modelKey(n.selection), Layer: subagent.RoleGroup(n.template), LayerSource: "derived", SelectionSource: "derived", Observed: n.observed, Finish: m.finish, EndedAt: m.ended, Requests: append([]RequestMetric(nil), m.requests...), title: n.title}
		if s.ParentID == "" {
			s.ParentID = m.parent
			s.Layer = subagent.RoleGroup(m.template)
			if m.parent == "" {
				s.Layer, s.LayerSource = "main", "measured"
			}
			s.ModelKey = modelKey(m.selection)
		}
		if n.layer != "" {
			s.Layer, s.LayerSource = viewer.SafeText(n.layer), "measured"
		} else if m.layer != "" {
			s.Layer, s.LayerSource = viewer.SafeText(m.layer), "measured"
		}
		s.StartedAt = m.created
		source := "derived"
		if parent := metrics[n.parent]; parent != nil {
			s.Review, s.RejectionEvents = parent.reviews[string(n.handle)].decision, parent.reviews[string(n.handle)].rejections
			if start, ok := parent.starts[string(n.handle)]; ok {
				a := start.activity
				s.StartedAt = a.StartedAt
				if s.StartedAt.IsZero() {
					s.StartedAt = start.recordedAt
				} else {
					source = "measured"
				}
				if a.Layer != "" {
					s.Layer, s.LayerSource = viewer.SafeText(a.Layer), "measured"
				}
				if a.Provider != "" && a.Model != "" {
					s.ModelKey, s.SelectionSource = ModelKey{viewer.SafeText(a.Provider), viewer.SafeText(a.Model), viewer.SafeText(a.Effort)}, "measured"
				}
			} else if at := parent.times[n.handle].start; !at.IsZero() {
				s.StartedAt = at
			}
			if s.EndedAt.IsZero() {
				s.EndedAt = parent.times[n.handle].end
				if !s.EndedAt.IsZero() {
					source = "derived"
				}
			}
		}
		if s.EndedAt.IsZero() && !n.archivedEnd.IsZero() {
			s.EndedAt, source = n.archivedEnd, "derived"
		}
		if n.status == operation.StatusCanceled {
			s.Finish = "canceled"
		} else if n.status == operation.StatusFailed {
			s.Finish = "failed"
		} else if s.Finish == "" && n.status == operation.StatusCompleted {
			s.Finish = "completed"
		}
		if s.SelectionSource != "measured" {
			for _, req := range s.Requests {
				if req.Kind == "response" && req.SelectionSource == "derived-from-responses" {
					s.ModelKey, s.SelectionSource = req.ModelKey, req.SelectionSource
					break
				}
			}
			if s.ModelKey == (ModelKey{}) {
				s.SelectionSource = "unknown"
			}
		}
		if s.Finish == "success" {
			s.Finish = "completed"
		}
		if s.Finish == "failure" {
			s.Finish = "failed"
		}
		wallSource := source
		if m.endSource == "derived" {
			wallSource = "derived"
		}
		s.WallTime = interval(s.StartedAt, s.EndedAt, wallSource)
		s.FirstResponse.Unknown++
		for _, req := range s.Requests {
			if req.Kind == "response" {
				s.FirstResponse = interval(s.StartedAt, req.At, source)
				break
			}
		}
		s.UsageLimitWait = m.waitMetric(n.observed)
		s.UsageRetryAt = m.waitRetryAt
		r.Sessions = append(r.Sessions, s)
	}
	r.buildTasks()
	r.aggregate()
	r.Routing = routing.sorted()
	r.ContextMeasurements = t.contextReport(root, r.Sessions)
	return r
}

func (r *TreeReport) buildTasks() {
	var workers []int
	for i, s := range r.Sessions {
		if s.Layer == "worker" {
			workers = append(workers, i)
		}
	}
	sort.SliceStable(workers, func(i, j int) bool {
		a, b := r.Sessions[workers[i]], r.Sessions[workers[j]]
		if a.StartedAt.Equal(b.StartedAt) {
			return a.ID < b.ID
		}
		return a.StartedAt.Before(b.StartedAt)
	})
	taskIndex := map[string]int{}
	firstStarts := map[string]time.Time{}
	previous := map[string][]int{}
	for _, i := range workers {
		s := &r.Sessions[i]
		key := string(s.ParentID) + "\x00" + s.title
		var prior *SessionMetric
		if s.title != "" && !s.StartedAt.IsZero() {
			for _, j := range previous[key] {
				p := &r.Sessions[j]
				if !p.EndedAt.IsZero() && !p.EndedAt.After(s.StartedAt) && (prior == nil || p.EndedAt.After(prior.EndedAt)) {
					prior = p
				}
			}
		}
		if prior != nil {
			prior.Redelegated = true
			s.Task = prior.Task
		} else {
			digest := sha256.Sum256([]byte(key + "\x00" + string(s.ID)))
			s.Task = fmt.Sprintf("task-%x", digest[:8])
			taskIndex[s.Task] = len(r.Tasks)
			r.Tasks = append(r.Tasks, TaskMetric{ModelKey: s.ModelKey, ID: s.Task, ParentID: s.ParentID})
			firstStarts[s.Task] = s.StartedAt
		}
		previous[key] = append(previous[key], i)
		task := &r.Tasks[taskIndex[s.Task]]
		task.Attempts++
		task.MixedModels = task.MixedModels || task.ModelKey != s.ModelKey
		task.Children = append(task.Children, s.ID)
		task.WallTime.merge(s.WallTime)
		for _, req := range s.Requests {
			mergeUsage(&task.Usage, req.Usage)
			task.TotalTokens.merge(totalTokens(req.Usage))
			task.MixedModels = task.MixedModels || req.ModelKey != s.ModelKey
		}
		if !s.Observed || len(s.Requests) == 0 {
			task.TotalTokens.Unknown++
			task.Usage.Partial = true
		}
		task.Elapsed = interval(firstStarts[s.Task], s.EndedAt, "derived")
	}
}

func totalTokens(u viewer.Usage) Metric {
	m := Metric{}
	if !u.Known || slices.Contains(u.Unknown, llm.UsageInput) || slices.Contains(u.Unknown, llm.UsageOutput) {
		m.Unknown++
	} else {
		m.add(float64(u.Input+u.Output), "measured")
	}
	return m
}

// mergeUsage preserves partial/unknown counters when combining request totals.
func mergeUsage(dst *viewer.Usage, src viewer.Usage) {
	dst.Known = dst.Known || src.Known
	dst.Partial = dst.Partial || src.Partial || !src.Known
	dst.Input += src.Input
	dst.Output += src.Output
	dst.CachedInput += src.CachedInput
	dst.CacheWriteInput += src.CacheWriteInput
	dst.Reasoning += src.Reasoning
	dst.Responses += src.Responses
	for _, f := range src.Unknown {
		if !slices.Contains(dst.Unknown, f) {
			dst.Unknown = append(dst.Unknown, f)
		}
	}
}

func (r *TreeReport) aggregate() {
	type layerKey struct {
		ModelKey
		layer, fast string
	}
	groups := map[layerKey]int{}
	group := func(layer string, model ModelKey, fast string) *LayerModel {
		key := layerKey{model, layer, fast}
		if i, ok := groups[key]; ok {
			return &r.Layers[i]
		}
		groups[key] = len(r.Layers)
		r.Layers = append(r.Layers, LayerModel{Layer: layer, ModelKey: model, Fast: fast})
		return &r.Layers[len(r.Layers)-1]
	}
	workers := map[ModelKey]int{}
	worker := func(model ModelKey) *WorkerSummary {
		if i, ok := workers[model]; ok {
			return &r.Workers[i]
		}
		workers[model] = len(r.Workers)
		r.Workers = append(r.Workers, WorkerSummary{ModelKey: model})
		return &r.Workers[len(r.Workers)-1]
	}
	for _, s := range r.Sessions {
		// Session timing belongs to the first response on its attributed model.
		// Each request below uses its own recorded mode, even after a switch.
		fast := ""
		for _, req := range s.Requests {
			if req.Kind == "response" && req.ModelKey == s.ModelKey {
				fast = req.Fast
				break
			}
		}
		g := group(s.Layer, s.ModelKey, fast)
		g.Sessions++
		if s.LayerSource == "measured" {
			g.RecordedLayers++
		} else {
			g.DerivedLayers++
		}
		if !s.Observed {
			g.Unobserved++
			g.Usage.Partial = true
			g.OutputTPS.Unknown++
			g.RequestDuration.Unknown++
			g.ListPriceUSD.Unknown++
		}
		if s.Observed && len(s.Requests) == 0 {
			g.Usage.Partial = true
			g.ListPriceUSD.Unknown++
		}
		if s.ParentID != "" {
			g.ChildWallTime.merge(s.WallTime)
			g.FirstResponse.merge(s.FirstResponse)
		}
		g.UsageLimitWait.merge(s.UsageLimitWait)
		for _, req := range s.Requests {
			g = group(s.Layer, req.ModelKey, req.Fast)
			g.Requests++
			mergeUsage(&g.Usage, req.Usage)
			g.ListPriceUSD.merge(req.ListPriceUSD)
			g.RequestDuration.merge(req.Duration)
			if req.OutputTPS.Known {
				g.timedOutput += req.TimedOutput
				g.generationSeconds += req.Duration.Value
				g.OutputTPS.Measured += req.OutputTPS.Measured
				g.OutputTPS.Derived += req.OutputTPS.Derived
			} else {
				g.OutputTPS.Unknown++
			}
		}
		if s.Layer == "worker" {
			w := worker(s.ModelKey)
			w.Attempts++
			if s.Redelegated {
				w.Rework.Redelegated++
			}
			w.Rework.RejectionEvents += s.RejectionEvents
			switch s.Finish {
			case "completed":
				w.Rework.Normal++
				if s.Review == "accept" || s.Review == "reject" {
					w.Rework.Reviewed++
					if s.Review == "accept" {
						w.Rework.Accepted++
					} else {
						w.Rework.Rejected++
					}
					if s.RejectionEvents > 0 || s.Redelegated {
						w.Rework.Reworked++
					}
				} else {
					w.Rework.Unreviewed++
				}
			case "failed":
				w.Rework.Failed++
			case "canceled":
				w.Rework.Canceled++
			default:
				w.Rework.UnknownEnd++
			}
		}
	}
	for i := range r.Layers {
		g := &r.Layers[i]
		if g.generationSeconds > 0 {
			g.OutputTPS.Known = true
			g.OutputTPS.Value = float64(g.timedOutput) / g.generationSeconds
		}
		g.RequestDuration = g.RequestDuration.average()
	}
	for _, task := range r.Tasks {
		w := worker(task.ModelKey)
		w.Tasks++
		if task.MixedModels {
			w.MixedModelTasks++
		}
		w.WallTime.merge(task.WallTime)
		w.Elapsed.merge(task.Elapsed)
		w.TotalTokens.merge(task.TotalTokens)
		mergeUsage(&w.Usage, task.Usage)
	}
	for i := range r.Workers {
		w := &r.Workers[i]
		if w.Rework.Reviewed > 0 {
			w.Rework.RejectRate.add(float64(w.Rework.Rejected)/float64(w.Rework.Reviewed), "measured")
			w.Rework.ReworkRate.add(float64(w.Rework.Reworked)/float64(w.Rework.Reviewed), "derived")
		} else {
			w.Rework.RejectRate.Unknown++
			w.Rework.ReworkRate.Unknown++
		}
		w.TimePerTask, w.TokensPerTask = w.WallTime, w.TotalTokens
		w.ElapsedPerTask = w.Elapsed
		if w.Tasks > 0 {
			w.TimePerTask.Value /= float64(w.Tasks)
			w.TokensPerTask.Value /= float64(w.Tasks)
			w.ElapsedPerTask.Value /= float64(w.Tasks)
			if w.TimePerTask.Known {
				w.TimePerTask.Measured, w.TimePerTask.Derived = 0, 1
			}
			if w.TokensPerTask.Known {
				w.TokensPerTask.Measured, w.TokensPerTask.Derived = 0, 1
			}
		}
	}
	sort.Slice(r.Layers, func(i, j int) bool {
		a, b := r.Layers[i], r.Layers[j]
		return layerOrder(a.Layer)+requestModelLabel(a.ModelKey, a.Fast) < layerOrder(b.Layer)+requestModelLabel(b.ModelKey, b.Fast)
	})
	sort.Slice(r.Workers, func(i, j int) bool { return modelLabel(r.Workers[i].ModelKey) < modelLabel(r.Workers[j].ModelKey) })
}

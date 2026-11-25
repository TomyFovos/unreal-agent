package subagent

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	"github.com/unreallabsai/unreal-agent/harness/usage"
)

const childQueued = "child_queued"
const childStarted = "child_start"

type SessionUsage struct {
	ID, Parent                   session.ID
	Handle                       operation.ID `json:",omitempty"`
	Title, Role, Template, State string       `json:",omitempty"`
	BudgetSteps                  int          `json:",omitempty"`
	Partial                      bool         `json:",omitempty"`
	Own, Subtree                 usage.Totals
}

func (s SessionUsage) UsageText() string {
	budget := "none"
	if s.BudgetSteps > 0 {
		budget = fmt.Sprint(s.BudgetSteps)
	}
	calls := fmt.Sprint(s.Subtree.Calls)
	if s.Partial {
		if s.Subtree.Calls == 0 {
			calls = "unknown"
		} else {
			calls += " (partial)"
		}
	}
	return fmt.Sprintf("calls %s / budget %s · charge %s", calls, budget, s.Subtree.DollarText())
}

type UsageWindows struct {
	All, Last10m, Last60m, Recent usage.Window
}

type usageWindowsBuilder struct {
	all, recent, last10, last60 usage.WindowBuilder
}

func (w *usageWindowsBuilder) add(role string, sample usage.Sample, now time.Time, since time.Duration) {
	w.all.Add(role, sample)
	if sample.At.IsZero() || sample.At.After(now) {
		return
	}
	age := now.Sub(sample.At)
	if age <= 10*time.Minute {
		w.last10.Add(role, sample)
	}
	if age <= time.Hour {
		w.last60.Add(role, sample)
	}
	if age <= since {
		w.recent.Add(role, sample)
	}
}

func (w *usageWindowsBuilder) windows() UsageWindows {
	return UsageWindows{All: w.all.Window(), Last10m: w.last10.Window(), Last60m: w.last60.Window(), Recent: w.recent.Window()}
}

type UsageReport struct {
	Root                          session.ID
	At                            time.Time
	Since                         string
	Partial                       bool
	All, Last10m, Last60m, Recent usage.Window
	Own                           UsageWindows
	CallsPerHour                  float64
	Allowances                    []usage.Allowance `json:",omitempty"`
	Spend                         []usage.Spend     `json:",omitempty"`
	Sessions, TopChildren         []SessionUsage    `json:",omitempty"`
}

// UsageReader caches only the usage projection. Its byte cursors follow
// immutable prefixes across archival and are independent of the Host's writer.
type UsageReader struct {
	mu    sync.Mutex
	store *localfile.Store
	nodes map[session.ID]*usageNode
}

func OpenUsageReader(directory string) (*UsageReader, error) {
	store, err := localfile.OpenExisting(directory)
	if err != nil {
		return nil, err
	}
	return &UsageReader{store: store, nodes: make(map[session.ID]*usageNode)}, nil
}

type usageNode struct {
	usage.Session
	cursor   localfile.UsageCursor
	children map[operation.ID]usageChild
	queued   map[operation.ID]uint64
	started  map[operation.ID]bool
	notices  map[inbox.ID]bool
}

type usageChild struct {
	plan   Plan
	status operation.Status
}

func (n *usageNode) update(id session.ID, op operation.Operation) {
	if op.Type != operation.TypeRemoteJob || op.ToolName != "" && op.ToolName != "SubagentStart" {
		return
	}
	if child, ok := n.children[op.ID]; ok {
		child.status = op.Status
		n.children[op.ID] = child
		return
	}
	p, err := DecodePlan(op)
	if err == nil && p.Action == "start" && p.ParentID == id {
		n.children[op.ID] = usageChild{p, op.Status}
	}
}

func (n *usageNode) apply(id session.ID, r localfile.UsageRecord) {
	switch r.Kind {
	case sessionstore.ItemFork:
		*n = usageNode{cursor: n.cursor, children: make(map[operation.ID]usageChild), queued: make(map[operation.ID]uint64),
			started: make(map[operation.ID]bool), notices: make(map[inbox.ID]bool)}
	case sessionstore.ItemTurn:
		n.Turn(r.Turn)
	}
	if r.Response != nil {
		n.Response(r.At, *r.Response)
	}
	if h := r.Host; h != nil {
		n.Host(r.At, *h)
		if h.Kind == "configuration" {
			var lineage struct {
				ParentID        session.ID
				Layer, Template string
			}
			if json.Unmarshal(h.Configuration, &lineage) == nil {
				n.Parent, n.Layer, n.Template = lineage.ParentID, lineage.Layer, lineage.Template
			}
		}
		if a := h.Activity; a != nil {
			switch a.Kind {
			case childQueued:
				n.queued[operation.ID(a.ID)] = a.QueueOrder
			case childStarted:
				n.started[operation.ID(a.ID)] = true
			}
		}
	}
	if input := r.Input; input != nil {
		switch input.Kind {
		case inbox.InputPeer:
			n.notices[input.ID] = true
		case inbox.InputControl:
			if op, err := sessionstore.DecodeOperationIntent(*input); err == nil {
				n.update(id, op)
			}
		}
	}
	for _, op := range r.Operations {
		n.update(id, op)
	}
}

func (r *UsageReader) readNode(ctx context.Context, id session.ID) (*usageNode, error) {
	n := r.nodes[id]
	if n == nil {
		n = &usageNode{}
		r.nodes[id] = n
	}
	next, err := r.store.ReadUsage(ctx, id, n.cursor, func() {
		*n = usageNode{children: make(map[operation.ID]usageChild), queued: make(map[operation.ID]uint64),
			started: make(map[operation.ID]bool), notices: make(map[inbox.ID]bool)}
	}, func(row localfile.UsageRecord) { n.apply(id, row) })
	n.cursor = next
	return n, err
}

type usageEntry struct {
	SessionUsage
	node    *usageNode
	started bool
}

func (r *UsageReader) readTree(ctx context.Context, root session.ID, activeOnly bool) ([]usageEntry, error) {
	entries := []usageEntry{{SessionUsage: SessionUsage{ID: root}}}
	seen := map[session.ID]bool{root: true}
	for i := 0; i < len(entries); i++ {
		entry := &entries[i]
		n, err := r.readNode(ctx, entry.ID)
		if err != nil {
			if i == 0 || ctx.Err() != nil {
				return nil, err
			}
			entry.Partial = !errors.Is(err, fs.ErrNotExist) || entry.started
			continue
		}
		entry.node = n
		if entry.Parent == "" {
			entry.Parent = n.Parent
		}
		if n.Layer != "" {
			entry.Role = n.Layer
		}
		if entry.Role == "" {
			entry.Role = "main"
			if entry.Parent != "" {
				entry.Role = RoleGroup(n.Template)
			}
		}
		var next []usageEntry
		for _, id := range slices.Sorted(maps.Keys(n.children)) {
			c := n.children[id]
			if activeOnly && terminalStatus(c.status) {
				continue
			}
			if seen[c.plan.ChildID] {
				entry.Partial = true
				continue
			}
			seen[c.plan.ChildID] = true
			layer := c.plan.Configuration.Layer
			if layer == "" {
				layer = ResolveLayer(c.plan.Template, entry.Role)
			}
			state := string(c.status)
			started := n.started[id] || c.status == operation.StatusCompleted || c.status == operation.StatusAwaiting && n.queued[id] == 0
			if n.queued[id] > 0 && !started && !terminalStatus(c.status) && c.status != operation.StatusCanceling {
				state = "queued"
			}
			next = append(next, usageEntry{SessionUsage: SessionUsage{ID: c.plan.ChildID, Parent: entry.ID, Handle: id,
				Title: childUsageTitle(c.plan), Role: layer, Template: c.plan.Template, State: state, BudgetSteps: c.plan.Brief.BudgetSteps}, started: started})
		}
		entries = append(entries, next...)
	}
	return entries, nil
}

// Read projects the caller's Session and its whole subtree, including archived
// children. All windows end at now, in the caller's local zone for Today.
func (r *UsageReader) Read(ctx context.Context, root session.ID, now time.Time, since time.Duration) (UsageReport, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entries, err := r.readTree(ctx, root, false)
	if err != nil {
		return UsageReport{}, err
	}
	report := UsageReport{Root: root, At: now, Since: since.String(), Sessions: make([]SessionUsage, len(entries))}
	var wholeTree, own usageWindowsBuilder
	spend := usage.NewSpendBuilder(now)
	var latest llm.SubscriptionObservations
	var history []llm.SubscriptionObservation
	tree := make([]usage.TotalsBuilder, len(entries))
	index := make(map[session.ID]int, len(entries))
	for i, entry := range entries {
		index[entry.ID] = i
		report.Sessions[i] = entry.SessionUsage
		if entry.node == nil {
			continue
		}
		latest.Merge(entry.node.Latest)
		history = append(history, entry.node.Allowance...)
		for sample := range entry.node.Samples {
			tree[i].Add(sample)
			wholeTree.add(entry.Role, sample, now, since)
			if i == 0 {
				own.add(entry.Role, sample, now, since)
			}
			spend.Add(sample)
		}
		report.Sessions[i].Own = tree[i].Totals()
	}
	for i := len(entries) - 1; i >= 0; i-- {
		row := &report.Sessions[i]
		row.Subtree = tree[i].Totals()
		if i > 0 {
			parent := index[row.Parent]
			tree[parent].Merge(row.Subtree)
			report.Sessions[parent].Partial = report.Sessions[parent].Partial || row.Partial
			if parent == 0 {
				report.TopChildren = append(report.TopChildren, *row)
			}
		}
	}
	slices.SortFunc(report.TopChildren, func(a, b SessionUsage) int {
		return cmp.Or(cmp.Compare(b.Subtree.Calls, a.Subtree.Calls), cmp.Compare(a.ID, b.ID))
	})
	windows := wholeTree.windows()
	report.All, report.Last10m, report.Last60m, report.Recent = windows.All, windows.Last10m, windows.Last60m, windows.Recent
	report.Own = own.windows()
	report.CallsPerHour = float64(report.Recent.Totals.Calls) / since.Hours()
	report.Partial = report.Sessions[0].Partial
	report.Allowances = usage.Allowances(now, latest, history)
	report.Spend = spend.Spend(report.Partial)
	return report, nil
}

func (p Plan) BudgetText() string {
	if p.Brief.BudgetSteps == 0 {
		return "none"
	}
	return fmt.Sprint(p.Brief.BudgetSteps)
}

func (p Plan) SelectionText() string {
	if p.Configuration != nil {
		if s := sessionstore.SelectionFromConfiguration(p.Configuration.Runtime); s != nil {
			return fmt.Sprintf("%s / %s / %s", usage.Label(s.Provider), usage.Label(s.Model), usage.Label(string(s.Effort)))
		}
	}
	return "unknown / unknown / unknown"
}

func childUsageTitle(p Plan) string {
	title, _, _ := strings.Cut(strings.TrimSpace(p.Title()), "\n")
	text := []rune(usage.Label(title))
	if len(text) > 120 {
		return string(text[:117]) + "..."
	}
	return string(text)
}

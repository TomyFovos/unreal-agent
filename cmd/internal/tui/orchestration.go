package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
	"github.com/unreallabsai/unreal-agent/internal/secretguard"
)

// ViewMode and its scroll positions are terminal presentation preferences. They
// are never submitted to Host or persisted with a Session.
type ViewMode string

const (
	ViewChat          ViewMode = "chat"
	ViewOrchestration ViewMode = "orchestration"
	ViewSplit         ViewMode = "split"
)

func (v ViewMode) title() string {
	switch v {
	case ViewOrchestration:
		return "Orchestration"
	case ViewSplit:
		return "Split"
	default:
		return "Chat"
	}
}

func (v ViewMode) effective(width int) ViewMode {
	if v == ViewSplit && width >= 120 {
		return ViewSplit
	}
	if v == ViewOrchestration {
		return v
	}
	return ViewChat
}

func viewPicker(mode ViewMode, width int) *Picker {
	p := &Picker{Kind: "view", Title: "View", Options: []string{"Chat", "Orchestration"}, Views: []ViewMode{ViewChat, ViewOrchestration}}
	if width >= 120 {
		p.Options = append(p.Options, "Split")
		p.Views = append(p.Views, ViewSplit)
	} else {
		p.Hint = "Split requires 120 columns"
	}
	for i, v := range p.Views {
		if v == mode.effective(width) {
			p.Selection = i
		}
	}
	return p
}

// OrchestrationSnapshot is a bounded, read-only frontend projection. It holds
// safe metadata only: no messages, tool state, runtime bindings or error bodies.
type OrchestrationSnapshot struct {
	Session   string
	Turn      string
	Nodes     []OrchestrationNode
	Edges     []OrchestrationEdge
	UpdatedAt time.Time
	Hidden    int
}

type OrchestrationNode struct {
	ID, ParentID, Kind, Label, Status                 string
	Provider, Model, ModelName, Effort, ObservedModel string
	RuntimeRevision                                   uint64
	RuntimeKnown                                      bool
	TaskLabel, ToolName, SafeTarget, Problem          string
	Elapsed                                           viewer.Duration
	Active                                            bool
}

type OrchestrationEdge struct {
	From, To, Kind string
	Owner          string // Unreal Host, not a provider-to-provider invocation
}

func topologyText(value string, cells int) string {
	if secretguard.Sensitive(value) {
		return "[protected]"
	}
	value = strings.Join(strings.Fields(SafeText(value)), " ")
	if secretguard.Sensitive(value) {
		return "[protected]"
	}
	l := textLine(value, normal)
	if l.width() > cells {
		return l.clip(max(0, cells-3)).plain() + "..."
	}
	return value
}

func agentNode(id session.ID, selection *sessionstore.RuntimeSelection) OrchestrationNode {
	n := OrchestrationNode{ID: "agent:" + string(id), Kind: "child_agent", Label: topologyText(string(id), 128), Status: "unknown"}
	if selection != nil {
		n.RuntimeKnown = true
		n.Provider = topologyText(selection.Provider, 128)
		n.Model = topologyText(selection.Model, 160)
		n.ModelName = topologyText(selection.Name, 160)
		n.Effort = topologyText(string(selection.Effort), 32)
		n.RuntimeRevision = selection.Revision
	}
	return n
}

func topologyChildStatus(r viewer.Row, s Snapshot) (string, bool) {
	if r.Finish != nil && r.Finish.Status == "completed" {
		return "done", false // canonical completion, not a guessed success report
	}
	_, word, _, ended := childState(r, s, Theme{ASCII: true})
	switch {
	case strings.Contains(word, "success"):
		return "done", false
	case strings.Contains(word, "failure") || strings.Contains(word, "failed") || strings.Contains(word, "error"):
		return "failed", false
	case word == "running" || word == "queued":
		return word, true
	case word == "canceled":
		return word, false
	case ended:
		return "stopped", false
	default:
		return "unknown", false
	}
}

func topologyOperationStatus(status operation.Status, uncertain bool) (string, bool) {
	if uncertain && !terminalStatus(status) {
		return "unknown", false
	}
	_, word, _ := stateMark(status, Theme{})
	if status == operation.StatusCompleted {
		word = "done"
	}
	return word, word == "running" || word == "queued"
}

// BuildOrchestration consumes indexed viewer state and the already bounded TUI
// snapshot. It never reads canonical history or schedules provider/Operation work.
// Active nodes sort before a bounded recent tail; rendering is O(node limit).
func BuildOrchestration(s Snapshot, panel viewer.PanelSnapshot, now time.Time, limit int) OrchestrationSnapshot {
	limit = max(1, min(limit, 512))
	out := OrchestrationSnapshot{Session: topologyText(string(s.ID), 128), Turn: topologyText(string(s.TurnID), 128), UpdatedAt: now}
	parent := agentNode(s.ID, s.Selection)
	parent.Kind, parent.Label, parent.Status = "parent_agent", "parent "+parent.Label, runtime(s)
	parent.Active = s.Connected && s.Running && (s.WaitingForModel || s.Progress != nil)
	if !s.Connected {
		parent.Problem = connection(s, now)
	}
	var main viewer.Row
	byID := map[session.ID]viewer.Row{}
	for _, r := range panel.Rows {
		if r.ID == s.ID {
			main = r
			if r.NeedsResync || r.Problem != "" {
				parent.Status, parent.Active, parent.Problem = "unknown", false, "resync required"
			}
			if r.Selection != nil && s.Selection != nil && r.Selection.Provider == s.Selection.Provider && r.Selection.Model == s.Selection.Model && r.Selection.Revision == s.Selection.Revision {
				parent.ObservedModel = topologyText(r.ObservedModel, 160)
			}
			parent.Elapsed = r.Elapsed
		} else if r.ID != "" && r.ParentID != "" {
			byID[r.ID] = r
		}
	}
	if parent.Status == "failed" {
		parent.Problem = "session failed"
		for i := len(s.Entries) - 1; i >= 0; i-- {
			switch s.Entries[i].Code {
			case "external_reauth_required":
				parent.Problem = "auth required"
			case "subscription_unavailable", "provider_unavailable":
				parent.Problem = "provider unavailable"
			case "rate_limited":
				parent.Problem = "provider rate limited"
			default:
				continue
			}
			break
		}
		// Provider errors expose a closed summary, not the SDK message/policy
		// body. This is a live failure projection, never canonical error data.
		if strings.Contains(s.Failure, "Claude tool bridge: provider permission denied (") || strings.Contains(s.Failure, "Claude Code: provider permission denied (") || strings.Contains(s.Failure, "Claude tool bridge allowlist is inactive") {
			parent.Problem = "provider permission denied"
		}
	}
	out.Nodes = append(out.Nodes, parent)
	rows := make([]viewer.Row, 0, len(byID))
	for _, r := range byID {
		rows = append(rows, r)
	}
	// Deterministic priority with lineage closure: retain an active grandchild's
	// ancestors rather than presenting it as a direct child of the main agent.
	sort.Slice(rows, func(i, j int) bool {
		_, ai := topologyChildStatus(rows[i], s)
		_, aj := topologyChildStatus(rows[j], s)
		if ai != aj {
			return ai
		}
		fi, fj := rows[i].Finish, rows[j].Finish
		if fi != nil && fj != nil && !fi.RecordedAt.Equal(fj.RecordedAt) {
			return fi.RecordedAt.After(fj.RecordedAt)
		}
		return rows[i].ID < rows[j].ID
	})
	chosen := map[session.ID]bool{s.ID: true}
	childLimit := min(max(8, limit/3), max(0, limit-1))
	for _, r := range rows {
		var chain []session.ID
		seen := map[session.ID]bool{}
		id := r.ID
		for !chosen[id] && !seen[id] {
			ancestor, ok := byID[id]
			if !ok {
				break
			}
			seen[id] = true
			chain = append(chain, id)
			id = ancestor.ParentID
		}
		if len(chosen)-1+len(chain) > childLimit {
			continue
		}
		for _, id := range chain {
			chosen[id] = true
		}
	}
	spawn := map[string]bool{}
	for _, r := range rows {
		spawn["agent:"+string(r.ParentID)+"/op:"+string(r.ParentOperationID)] = true
		if !chosen[r.ID] {
			out.Hidden++
			continue
		}
		n := agentNode(r.ID, r.Selection)
		n.ParentID = "agent:" + string(r.ParentID)
		n.Status, n.Active = topologyChildStatus(r, s)
		n.TaskLabel = topologyText(r.Label, 160)
		n.ObservedModel = topologyText(r.ObservedModel, 160)
		n.Elapsed = r.Elapsed
		if r.NeedsResync || main.NeedsResync || r.Problem != "" {
			n.Problem = "resync required"
			if r.Finish == nil {
				n.Status, n.Active = "unknown", false
			}
		}
		out.Nodes = append(out.Nodes, n)
		out.Edges = append(out.Edges, OrchestrationEdge{From: n.ParentID, To: n.ID, Kind: "child_request", Owner: "Unreal Host"})
	}
	// Parent operation status comes from the latest Host snapshot; display-only
	// target/elapsed linkage comes from viewer or bounded Live Dock receipts.
	parentOps := map[operation.ID]viewer.OperationRow{}
	for _, op := range main.Operations {
		parentOps[op.ID] = op
	}
	for _, op := range s.Operations {
		p := parentOps[op.ID]
		p.ID, p.Tool, p.Type, p.Status = op.ID, op.ToolName, op.Type, op.Status
		parentOps[op.ID] = p
	}
	for _, e := range s.Entries {
		for _, receipt := range e.Calls {
			for _, id := range receipt.Operations {
				if op, ok := parentOps[id]; ok && op.Target == "" {
					op.Target, op.Sequence = receipt.Target, e.Sequence
					parentOps[id] = op
				}
			}
		}
	}
	var ops []OrchestrationNode
	sequences := map[string]sessionstore.Sequence{}
	addOps := func(id session.ID, operations []viewer.OperationRow, uncertain bool) {
		for _, op := range operations {
			nid := "agent:" + string(id) + "/op:" + string(op.ID)
			if spawn[nid] {
				continue
			} // Child edge already represents this request.
			n := OrchestrationNode{ID: nid, ParentID: "agent:" + string(id), Kind: "operation", Label: topologyText(string(op.ID), 128), ToolName: topologyText(op.Tool, 128), SafeTarget: topologyText(op.Target, 160), Elapsed: op.Elapsed}
			if n.ToolName == "" {
				n.ToolName = topologyText(string(op.Type), 128)
			}
			if n.ToolName == "" {
				n.ToolName = "operation"
			}
			n.Status, n.Active = topologyOperationStatus(op.Status, uncertain)
			if uncertain && !terminalStatus(op.Status) {
				n.Problem = "observation unknown"
				n.Elapsed = viewer.Duration{}
			}
			ops = append(ops, n)
			sequences[nid] = op.Sequence
		}
	}
	var po []viewer.OperationRow
	for _, op := range parentOps {
		po = append(po, op)
	}
	addOps(s.ID, po, !s.Connected || main.NeedsResync || main.Problem != "")
	for _, r := range rows {
		if chosen[r.ID] {
			addOps(r.ID, r.Operations, !s.Connected || r.NeedsResync || main.NeedsResync || !observed(r))
		}
	}
	sort.Slice(ops, func(i, j int) bool {
		if ops[i].Active != ops[j].Active {
			return ops[i].Active
		}
		// Sequences are only comparable within an agent's own canonical history.
		if ops[i].ParentID == ops[j].ParentID && sequences[ops[i].ID] != sequences[ops[j].ID] {
			return sequences[ops[i].ID] > sequences[ops[j].ID]
		}
		return ops[i].ID < ops[j].ID
	})
	for _, n := range ops {
		if len(out.Nodes) == limit {
			out.Hidden++
			continue
		}
		out.Nodes = append(out.Nodes, n)
		out.Edges = append(out.Edges, OrchestrationEdge{From: n.ParentID, To: n.ID, Kind: "tool_request", Owner: "Unreal Host"})
	}
	return out
}

type topologyRow struct {
	content line
	active  bool
}

func topologyMark(status string, t Theme) (string, style) {
	switch status {
	case "running":
		return t.symbol("▸", ">"), liveStyle
	case "queued":
		return t.symbol("◌", "."), meta
	case "done":
		return t.symbol("✓", "+"), successStyle
	case "failed":
		return t.symbol("✗", "x"), failureStyle
	case "canceled":
		return t.symbol("⊘", "-"), normal
	case "stopped":
		return t.symbol("▪", "#"), meta
	default:
		return "?", warningStyle
	}
}

func topologyRows(s OrchestrationSnapshot, width int, t Theme) []topologyRow {
	byID := map[string]OrchestrationNode{}
	children := map[string][]string{}
	for _, n := range s.Nodes {
		byID[n.ID] = n
		children[n.ParentID] = append(children[n.ParentID], n.ID)
	}
	active := map[string]bool{}
	for _, n := range s.Nodes {
		if !n.Active {
			continue
		}
		seen := map[string]bool{}
		for id := n.ID; id != "" && !seen[id]; id = byID[id].ParentID {
			seen[id], active[id] = true, true
		}
	}
	out := []topologyRow{{content: textLine("Orchestration", strong).clip(width)}, {content: textLine("Unreal Host validates / owns requests", meta).clip(width)}}
	seen := map[string]bool{}
	var walk func(string, string, bool, int)
	walk = func(id, prefix string, last bool, depth int) {
		if seen[id] {
			return
		}
		seen[id] = true
		n := byID[id]
		branch, continuation := "", ""
		if depth > 0 {
			branch, continuation = t.symbol("├─ ", "+- "), t.symbol("│  ", "|  ")
			if last {
				branch, continuation = t.symbol("└─ ", "+- "), "   "
			}
		}
		prefix = textLine(prefix, meta).clip(max(0, width/3)).plain()
		mark, st := topologyMark(n.Status, t)
		bodyStyle := normal
		if active[id] {
			bodyStyle = strong
		} else if n.Status == "done" || n.Status == "stopped" {
			bodyStyle = meta
		}
		name := n.Label
		if n.Kind == "operation" {
			name = n.ToolName
		}
		left := textLine(prefix+branch, meta)
		head := joined(left, textLine(mark+" ", st), textLine(name, bodyStyle))
		if n.SafeTarget != "" {
			head = joined(head, textLine("  "+n.SafeTarget, normal))
		}
		suffix := "  " + n.Status
		if n.Elapsed.Known {
			suffix += " " + duration(n.Elapsed.Value)
		}
		if head.width()+textLine(suffix, st).width() > width {
			head = head.clip(max(left.width()+2, width-textLine(suffix, st).width()))
		}
		out = append(out, topologyRow{content: joined(head, textLine(suffix, st)).clip(width), active: active[id]})
		indent := prefix + continuation + "  "
		add := func(value string, style style) {
			if value != "" {
				out = append(out, topologyRow{content: joined(textLine(indent, meta), textLine(value, style)).clip(width)})
			}
		}
		if n.Kind != "operation" {
			value := "provider / model / effort: unknown"
			if n.RuntimeKnown {
				provider := n.Provider
				if provider == "" {
					provider = "unknown"
				}
				selection := sessionstore.RuntimeSelection{Provider: provider, Model: n.Model, Name: n.ModelName}
				value = selectionLabel(selection, t)
				if n.Model == "" {
					value += "model unknown"
				}
				if n.Effort != "" {
					value += t.symbol(" · ", " / ") + n.Effort
				}
				value += fmt.Sprintf("  r%d", n.RuntimeRevision)
			}
			add(value, meta)
			if n.Kind == "parent_agent" && s.Turn != "" {
				add("turn: "+s.Turn, meta)
			}
			if n.ObservedModel != "" && n.ObservedModel != n.Model {
				add("observed: "+n.ObservedModel, meta)
			}
			if n.TaskLabel != "" {
				add("task: "+n.TaskLabel, normal)
			}
		}
		add(n.Problem, warningStyle)
		for i, child := range children[id] {
			walk(child, prefix+continuation, i == len(children[id])-1, depth+1)
		}
	}
	for _, id := range children[""] {
		walk(id, "", true, 0)
	}
	for _, n := range s.Nodes {
		if !seen[n.ID] {
			out = append(out, topologyRow{content: textLine("? lineage unknown (cycle or missing parent)", warningStyle).clip(width)})
			walk(n.ID, "", true, 0)
		}
	}
	if s.Hidden > 0 {
		out = append(out, topologyRow{content: textLine(fmt.Sprintf("%d older/overflow nodes hidden; /analyze Agents", s.Hidden), meta).clip(width)})
	}
	return out
}

// Orchestration scroll is top-relative. Split follows the first active child or
// Operation, keeping the parent and Host ownership caption visible above it.
func orchestrationPane(snapshot OrchestrationSnapshot, u UIState, width, height int, follow bool) ([]line, int, int) {
	if height <= 0 {
		return nil, 0, 0
	}
	rows := topologyRows(snapshot, width, u.Theme)
	maxOffset := max(0, len(rows)-height)
	start := min(max(0, u.OrchestrationScroll), maxOffset)
	if follow {
		start = 0
		for i := 4; i < len(rows); i++ {
			if rows[i].active {
				start = i
				break
			}
		}
		// Keep parent runtime and Host ownership visible when following a later
		// active path. No second pane scroll mode or keyboard ownership is added.
		if start > 4 && height > 6 {
			var out []line
			for _, r := range rows[:4] {
				out = append(out, r.content)
			}
			out = append(out, textLine(u.Theme.symbol("↑ ", "^ ")+"active path; /view orchestration for all", meta).clip(width))
			start = min(start, max(4, len(rows)-(height-len(out))))
			for _, r := range rows[start:min(len(rows), start+height-len(out))] {
				out = append(out, r.content)
			}
			return out, start, maxOffset
		}
		start = 0
	}
	var out []line
	for _, row := range rows[start:min(len(rows), start+height)] {
		out = append(out, row.content)
	}
	return out, min(start, maxOffset), maxOffset
}

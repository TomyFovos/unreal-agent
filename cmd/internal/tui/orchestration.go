package tui

import (
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
// safe metadata and each running child's latest activity, the short excerpt
// the Live Dock shows: no full messages, tool state, runtime bindings or error
// bodies.
type OrchestrationSnapshot struct {
	Session   string
	Turn      string
	Nodes     []OrchestrationNode
	UpdatedAt time.Time
	Hidden    int
	cards     []workCard
	beads     []workBead
}

type OrchestrationNode struct {
	ID, ParentID, Kind, Label, Status                 string
	Provider, Model, ModelName, Effort, ObservedModel string
	RuntimeRevision                                   uint64
	RuntimeKnown                                      bool
	TaskLabel, ToolName, SafeTarget, Problem          string
	Activity                                          string // what a running child does now
	Elapsed                                           viewer.Duration
	Active                                            bool
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
	return topologyState(childVisualState(r, s))
}

func topologyState(state visualState) (string, bool) {
	switch state {
	case stateCompleted:
		return "done", false
	case stateFailed:
		return "failed", false
	case stateWorking:
		return "running", true
	case stateCanceling:
		return "canceling", true
	case stateApproval:
		return "approval", false
	case stateCanceled:
		return "canceled", false
	case stateEnded:
		return "stopped", false
	default:
		return "unknown", false
	}
}

func topologyOperationStatus(status operation.Status, uncertain bool) (string, bool) {
	if uncertain && !terminalStatus(status) {
		return "unknown", false
	}
	return topologyState(operationVisualState(status))
}

// BuildOrchestration consumes indexed viewer state and the already bounded TUI
// snapshot. It never reads canonical history or schedules provider/Operation work.
// Active nodes sort before a bounded recent tail; rendering is O(node limit).
func BuildOrchestration(s Snapshot, panel viewer.PanelSnapshot, now time.Time, limit int) OrchestrationSnapshot {
	limit = max(1, min(limit, 512))
	out := OrchestrationSnapshot{Session: topologyText(string(s.ID), 128), Turn: topologyText(string(s.TurnID), 128), UpdatedAt: now}
	out.cards, out.beads = buildWorkCards(s, panel, now)
	parent := agentNode(s.ID, s.Selection)
	parent.Kind, parent.Label, parent.Status = "parent_agent", "parent "+parent.Label, runtime(s)
	parent.Active = s.Connected && s.Running && (s.WaitingForModel || s.Progress != nil)
	if !s.Connected {
		parent.Problem = connection(s, now)
	}
	var main viewer.Row
	byID := map[session.ID]viewer.Row{}
	for _, r := range children(UIState{Viewer: panel}, s) {
		byID[r.ID] = r
	}
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
		if observed(r) && r.Finish == nil {
			n.Activity = topologyText(r.Activity, 160)
		}
		n.ObservedModel = topologyText(r.ObservedModel, 160)
		n.Elapsed = r.Elapsed
		if r.NeedsResync || main.NeedsResync || r.Problem != "" {
			n.Problem = "resync required"
			if r.Finish == nil {
				n.Status, n.Active = "unknown", false
			}
		}
		out.Nodes = append(out.Nodes, n)
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
	}
	return out
}

// Orchestration scroll remains top-relative. Split and Chat use the same
// cards with a compact viewport and an explicit count of cards below it.
func orchestrationPane(snapshot OrchestrationSnapshot, u UIState, width, height int, follow bool) ([]line, int, int) {
	if height <= 0 {
		return nil, 0, 0
	}
	return workCardPane(snapshot, u, width, height, follow)
}

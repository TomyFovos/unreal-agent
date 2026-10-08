// Package analysis derives metadata/statistics from public canonical history.
// Reports contain no input/output bodies, tool arguments or credential fields.
package analysis

import (
	jsonv1 "encoding/json"
	"encoding/json/v2"
	"slices"
	"sort"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/provider"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
)

type Selection struct {
	Provider, Model, Name, Effort string
	Revision                      uint64
	ContextWindow                 int64 `json:",omitzero"`
}
type Turn struct {
	ID            session.TurnID
	Number        int
	At            time.Time
	Selection     *Selection `json:",omitzero"`
	Usage         viewer.Usage
	ObservedModel string `json:",omitzero"`
}
type Counts struct {
	Calls, Completed, Failed, Canceled, Unresolved int
	Elapsed                                        viewer.Duration
	ElapsedPartial                                 bool
}
type Tool struct {
	Name string
	Counts
}
type Agent struct {
	Selection     *Selection `json:",omitzero"`
	ObservedModel string     `json:",omitzero"`
	ID            session.ID
	Observed      bool
	Runtime       viewer.Liveness
	ParentStatus  operation.Status
	Finish        string
	Usage         viewer.Usage
	Elapsed       viewer.Duration
	Operations    *int `json:",omitzero"`
}
type AgentCounts struct {
	Discovered, Observed, Success, Failed, Canceled, UnknownFinish int
}
type Event struct {
	At         time.Time
	Kind, Name string
}
type Errors struct {
	Provider, Tools, Children, Crashes int
	CrashesKnown                       bool
	Recent                             []Event
}
type Report struct {
	Version           int
	Session           session.ID
	At, CreatedAt     time.Time
	Partial, Resync   bool
	Selection         *Selection `json:",omitzero"`
	Pending           *Selection `json:",omitzero"`
	ManagedPolicyMode string     `json:"managedPolicyMode,omitzero"`
	ToolCapability    string     `json:",omitzero"`
	ToolBridgeEnabled bool       `json:",omitzero"`
	ToolBridgeMode    string     `json:",omitzero"`
	ToolBridgeStatus  string     `json:",omitzero"` // live registration, not a permission grant
	Elapsed           viewer.Duration
	Usage             viewer.Usage
	Turns             []Turn
	Tools             Counts
	ByTool            []Tool
	Agents            []Agent
	AgentCounts       AgentCounts
	Context           []Turn
	ContextPackage    *contextengine.Diagnostics `json:",omitzero"`
	ContextWindow     int64                      `json:",omitzero"`
	Timeline          []Event
	Errors            Errors
}

// JSON duration values are nanoseconds. Explicit encoding keeps the projection
// independent of json/v2's deliberately unspecified default duration format.
func (r Report) MarshalJSON() ([]byte, error) {
	type plain Report
	return json.Marshal(plain(r), jsonv1.FormatDurationAsNano(true))
}

type call struct {
	name       string
	start, end time.Time
	status     *sessionstore.ToolCallStatus
}
type Accumulator struct {
	id                session.ID
	created           time.Time
	usage             viewer.Usage
	turns             []Turn
	turnIndex         map[session.TurnID]int
	active, pending   *sessionstore.RuntimeSelection
	managedPolicyMode string
	toolBridgeEnabled bool
	toolBridgeMode    string
	calls             map[string]*call
	operations        map[operation.ID]operation.Operation
	events, errors    []Event
	providerErrors    int
	crashes           int
	partial           bool
}

func New(id session.ID) *Accumulator {
	return &Accumulator{id: id, turnIndex: map[session.TurnID]int{}, calls: map[string]*call{}, operations: map[operation.ID]operation.Operation{}}
}
func summarize(s *sessionstore.RuntimeSelection) *Selection {
	if s == nil {
		return nil
	}
	return &Selection{Provider: viewer.SafeText(s.Provider), Model: viewer.SafeText(s.Model), Name: viewer.SafeText(s.Name), Effort: string(s.Effort), Revision: s.Revision, ContextWindow: s.ContextWindow}
}
func (a *Accumulator) Created(at time.Time) { a.created = at }
func (a *Accumulator) Apply(item host.HistoryItem) {
	e := Event{At: item.RecordedAt}
	switch v := item.Data.(type) {
	case sessionstore.HostRecord:
		switch v.Kind {
		case "configuration":
			if a.active == nil {
				a.active = sessionstore.SelectionFromConfiguration(v.Configuration)
			}
			a.managedPolicyMode = sessionstore.ManagedPolicyModeFromConfiguration(v.Configuration)
			a.toolBridgeEnabled = sessionstore.ToolBridgeEnabledFromConfiguration(v.Configuration)
			a.toolBridgeMode = sessionstore.ToolBridgeModeFromConfiguration(v.Configuration)
		case sessionstore.HostRuntimeSelection:
			a.pending = v.Selection
			e.Kind = "model selection queued"
			if v.Selection != nil {
				e.Name = viewer.SafeText(v.Selection.Model)
			}
		case sessionstore.HostRuntimeApplied:
			a.active = v.Selection
			if v.Selection != nil && v.Selection.Binding != "" {
				a.managedPolicyMode = sessionstore.ManagedPolicyModeFromConfiguration([]byte(v.Selection.Binding))
				a.toolBridgeEnabled = sessionstore.ToolBridgeEnabledFromConfiguration([]byte(v.Selection.Binding))
				a.toolBridgeMode = sessionstore.ToolBridgeModeFromConfiguration([]byte(v.Selection.Binding))
			} else if v.Selection != nil && v.Selection.Provider != "claude-code" {
				a.managedPolicyMode = ""
				a.toolBridgeEnabled = false
				a.toolBridgeMode = ""
			}
			a.pending = nil
			e.Kind = "model selection applied"
			if v.Selection != nil {
				e.Name = viewer.SafeText(v.Selection.Model)
			}
		case "finish":
			e.Kind = "canonical finish"
		}
	case sessionstore.Fork:
		a.active, a.pending = nil, nil
		a.managedPolicyMode = ""
		a.toolBridgeEnabled = false
		a.toolBridgeMode = ""
		e.Kind = "fork"
	case inbox.Input:
		if v.Kind == inbox.InputExternal {
			e.Kind = "user input"
		} else if v.Kind == inbox.InputPeer {
			e.Kind = "peer input"
		} else if v.Kind == inbox.InputCrash {
			a.crashes++
			e.Kind = "canonical crash"
			a.addError(Event{At: item.RecordedAt, Kind: "crash", Name: "canonical crash record"})
		} else {
			e.Kind = "control"
			if control, err := v.DecodeControlMessage(); err == nil && control.Mode == inbox.UpdateSettings && a.active != nil {
				choice := *a.active
				choice.Effort = control.Parameters.(inbox.Settings).ReasoningEffort
				a.active = &choice
				e.Kind = "reasoning settings"
			}
			if op, err := sessionstore.DecodeOperationIntent(v); err == nil {
				e.Kind, e.Name = "operation intent", viewer.SafeText(op.ToolName)
			}
		}
	case session.Turn:
		if len(a.turns) >= 4096 {
			a.partial = true
			break
		}
		t := Turn{ID: v.ID, Number: len(a.turns) + 1, At: item.RecordedAt}
		if a.active != nil && a.active.Revision == v.RuntimeRevision {
			t.Selection = summarize(a.active)
		} else {
			a.partial = true
		}
		a.turnIndex[v.ID] = len(a.turns)
		a.turns = append(a.turns, t)
	case sessionstore.ModelResponse:
		viewer.AddUsage(&a.usage, v.Response.Usage)
		if index, ok := a.turnIndex[v.TurnID]; ok {
			viewer.AddUsage(&a.turns[index].Usage, v.Response.Usage)
			a.turns[index].ObservedModel = viewer.SafeText(v.Response.Model)
		} else {
			a.partial = true
		}
		e.Kind = "model response"
		if v.Response.Failure != nil {
			a.providerErrors++
			a.addError(Event{At: item.RecordedAt, Kind: "provider", Name: "canonical model failure"})
		}
		for _, out := range v.Response.Output {
			if tc, ok := out.Data.(llm.ToolCall); ok {
				if len(a.calls) >= 65536 {
					a.partial = true
					continue
				}
				key := string(v.TurnID) + "\x00" + tc.CallID
				a.calls[key] = &call{name: viewer.SafeText(tc.Name), start: item.RecordedAt}
				a.addEvent(Event{At: item.RecordedAt, Kind: "tool call", Name: viewer.SafeText(tc.Name)})
			}
		}
	case sessionstore.ToolCallStatus:
		key := string(v.TurnID) + "\x00" + v.CallID
		if c := a.calls[key]; c != nil {
			copy := v
			c.status = &copy
			for _, op := range v.Operations {
				prior, exists := a.operations[op.ID]
				if !exists || prior.Status != op.Status {
					a.addEvent(Event{At: item.RecordedAt, Kind: "operation " + string(op.Status), Name: viewer.SafeText(op.ToolName)})
				}
				a.operations[op.ID] = op
			}
			if terminalCall(c, a.operations) != "" {
				c.end = item.RecordedAt
			}
		}
		e.Kind = "tool status"
	}
	if e.Kind != "" {
		a.addEvent(e)
	}
}
func (a *Accumulator) addEvent(e Event) {
	if e.At.IsZero() {
		return
	}
	a.events = append(a.events, e)
	if len(a.events) > 512 {
		a.events = a.events[len(a.events)-512:]
		a.partial = true
	}
}
func (a *Accumulator) addError(e Event) {
	a.errors = append(a.errors, e)
	if len(a.errors) > 128 {
		a.errors = a.errors[len(a.errors)-128:]
		a.partial = true
	}
}
func terminalCall(c *call, ops map[operation.ID]operation.Operation) string {
	if c.status == nil {
		return ""
	}
	if c.status.Status.Error != "" {
		return "failed"
	}
	status := "completed"
	for _, id := range c.status.Status.WaitingFor {
		op, ok := ops[id]
		if !ok {
			return ""
		}
		switch op.Status {
		case operation.StatusFailed:
			status = "failed"
		case operation.StatusCanceled:
			if status != "failed" {
				status = "canceled"
			}
		case operation.StatusCompleted:
		default:
			return ""
		}
	}
	return status
}
func count(c *Counts, status string, d viewer.Duration) {
	c.Calls++
	switch status {
	case "completed":
		c.Completed++
	case "failed":
		c.Failed++
	case "canceled":
		c.Canceled++
	default:
		c.Unresolved++
	}
	if d.Known {
		c.Elapsed.Known = true
		c.Elapsed.Value += d.Value
	} else {
		c.ElapsedPartial = true
	}
}
func (a *Accumulator) Snapshot(now time.Time, _ bool, resync bool, rows []viewer.Row, ops []operation.Operation) Report {
	r := Report{Version: 1, Session: a.id, At: now, CreatedAt: a.created, Partial: a.partial || resync, Resync: resync, Selection: summarize(a.active), Pending: summarize(a.pending), Usage: a.usage, Turns: append([]Turn(nil), a.turns...), Timeline: append([]Event(nil), a.events...), Errors: Errors{Provider: a.providerErrors, Crashes: a.crashes, Recent: append([]Event(nil), a.errors...)}}
	r.ToolCapability = "unknown"
	if a.active != nil {
		for _, d := range provider.Defaults(nil) {
			if d.ID == a.active.Provider {
				r.ToolCapability = "text-only"
				if slices.Contains(d.Capabilities, "tools") {
					r.ToolCapability = "tools supported"
				}
			}
		}
	}
	r.ManagedPolicyMode = a.managedPolicyMode
	r.ToolBridgeEnabled = a.toolBridgeEnabled
	r.ToolBridgeMode = a.toolBridgeMode
	if r.ToolBridgeEnabled {
		r.ToolCapability = "Unreal tools via SDK MCP"
		if r.ToolBridgeMode == "structured" {
			r.ToolCapability = "Unreal tools via structured actions"
		}
	}
	r.Usage.Partial = r.Usage.Partial || r.Partial
	for _, op := range ops {
		a.operations[op.ID] = op
	}
	byTool := map[string]*Tool{}
	for _, c := range a.calls {
		status := terminalCall(c, a.operations)
		d := viewer.Duration{}
		if !c.start.IsZero() && !c.end.IsZero() && !c.end.Before(c.start) {
			d = viewer.Duration{Known: true, Value: c.end.Sub(c.start)}
		}
		count(&r.Tools, status, d)
		if byTool[c.name] == nil {
			byTool[c.name] = &Tool{Name: c.name}
		}
		count(&byTool[c.name].Counts, status, d)
		if status == "failed" || status == "canceled" {
			r.Errors.Tools++
			r.Errors.Recent = append(r.Errors.Recent, Event{At: c.end, Kind: "tool", Name: c.name + " " + status})
		}
	}
	for _, t := range byTool {
		r.ByTool = append(r.ByTool, *t)
	}
	sort.Slice(r.ByTool, func(i, j int) bool { return r.ByTool[i].Name < r.ByTool[j].Name })
	for _, row := range rows {
		if row.ID == a.id {
			r.Elapsed = row.Elapsed
			r.Partial = r.Partial || row.More || row.NeedsResync
			r.Resync = r.Resync || row.NeedsResync
			r.Usage.Partial = r.Usage.Partial || r.Partial
			continue
		}
		if row.ParentID != a.id {
			continue
		}
		agent := Agent{ID: row.ID, Observed: row.Cursor > 0 && !row.NeedsResync && !row.More, Runtime: row.Runtime, ParentStatus: row.ParentOperationStatus, Elapsed: row.Elapsed, Usage: row.Usage, Selection: summarize(row.Selection), ObservedModel: viewer.SafeText(row.ObservedModel)}
		r.AgentCounts.Discovered++
		if agent.Observed {
			r.AgentCounts.Observed++
			count := len(row.Operations)
			agent.Operations = &count
		} else {
			agent.Usage = viewer.Usage{Partial: true}
		}
		if row.Finish != nil {
			agent.Finish = viewer.SafeText(row.Finish.Status)
			if !row.Finish.RecordedAt.IsZero() {
				r.Timeline = append(r.Timeline, Event{At: row.Finish.RecordedAt, Kind: "child finished", Name: viewer.SafeText(string(row.ID)) + " " + agent.Finish})
			}
		}
		switch {
		case agent.Finish == "success" || agent.Finish == "completed":
			r.AgentCounts.Success++
		case agent.Finish == "failure" || agent.Finish == "failed" || agent.ParentStatus == operation.StatusFailed:
			r.AgentCounts.Failed++
		case agent.ParentStatus == operation.StatusCanceled:
			r.AgentCounts.Canceled++
		default:
			r.AgentCounts.UnknownFinish++
		}
		if agent.Finish == "failure" || agent.ParentStatus == operation.StatusFailed {
			r.Errors.Children++
			at := time.Time{}
			if row.Finish != nil {
				at = row.Finish.RecordedAt
			}
			r.Errors.Recent = append(r.Errors.Recent, Event{At: at, Kind: "child failure", Name: viewer.SafeText(string(row.ID))})
		}
		r.Agents = append(r.Agents, agent)
	}
	if a.active != nil {
		r.ContextWindow = a.active.ContextWindow
	}
	for _, turn := range r.Turns {
		if turn.Usage.Known && !(turn.Usage.Input == 0 && slices.Contains(turn.Usage.Unknown, llm.UsageInput)) {
			r.Context = append(r.Context, turn)
		}
	}
	sort.SliceStable(r.Timeline, func(i, j int) bool { return r.Timeline[i].At.Before(r.Timeline[j].At) })
	if len(r.Timeline) > 512 {
		r.Timeline = r.Timeline[len(r.Timeline)-512:]
		r.Partial = true
	}
	// Calls are keyed by ID, so map iteration cannot define recency. Sort the
	// observed timestamps before bounding the recent view; retain exact counts.
	sort.Slice(r.Errors.Recent, func(i, j int) bool {
		x, y := r.Errors.Recent[i], r.Errors.Recent[j]
		if !x.At.Equal(y.At) {
			return x.At.Before(y.At)
		}
		if x.Kind != y.Kind {
			return x.Kind < y.Kind
		}
		return x.Name < y.Name
	})
	if len(r.Errors.Recent) > 128 {
		r.Errors.Recent = r.Errors.Recent[len(r.Errors.Recent)-128:]
		r.Partial = true
	}
	r.Errors.CrashesKnown = !r.Partial
	r.Usage.Partial = r.Usage.Partial || r.Partial
	return r
}

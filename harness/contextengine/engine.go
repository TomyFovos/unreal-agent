package contextengine

import (
	"cmp"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"slices"
	"sync"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

// Engine is a replayable ordered index. It never selects, retrieves or evicts.
type Engine struct {
	mu         sync.Mutex
	units      map[string]Unit
	order      []string
	sources    map[string][]string
	state      State
	ops        map[string]OperationState
	through    sessionstore.Sequence
	latestUser string
}

func New(c Config) (*Engine, error) {
	if _, err := c.Resolve(); err != nil {
		return nil, err
	}
	return &Engine{units: map[string]Unit{}, sources: map[string][]string{}, ops: map[string]OperationState{}}, nil
}

func (e *Engine) Configure(r Runtime, c Constraints, instructions bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.state.Runtime, e.state.Constraints, e.state.ProjectInstructionsBound = r, c, instructions
	e.state.Constraints.AllowedTools = slices.Clone(c.AllowedTools)
}

func (e *Engine) Add(u Unit, source string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if u.Kind == UserMessage && u.Required {
		if previous, ok := e.units[e.latestUser]; ok {
			previous.Required = false
			e.units[e.latestUser] = previous
		}
	}
	u.ID = fmt.Sprintf("unit-%020d", len(e.order)+1)
	public, ok := PublicItem(u.Item)
	if !ok || u.Class == Omit {
		u.Class = Omit
		u.Item = llm.Item{}
	} else {
		u.Item = public
	}
	e.units[u.ID] = u
	if u.Kind == UserMessage && u.Required {
		e.latestUser = u.ID
	}
	e.order = append(e.order, u.ID)
	e.sources[source] = append(e.sources[source], u.ID)
	return u.ID
}

func (e *Engine) Observe(item sessionstore.Item, source string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, id := range e.sources[source] {
		u := e.units[id]
		u.Source.Sequence, u.Source.RecordedAt = item.Sequence, item.RecordedAt
		e.units[id] = u
	}
	delete(e.sources, source)
	e.through = max(e.through, item.Sequence)
	if s, ok := item.Data.(sessionstore.ToolCallStatus); ok {
		for _, op := range s.Operations {
			e.observeOperation(op)
		}
	}
}

func (e *Engine) ObserveOperation(op operation.Operation) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.observeOperation(op)
}
func (e *Engine) observeOperation(op operation.Operation) {
	v := OperationState{ID: string(op.ID), Type: string(op.Type), Tool: op.ToolName, Status: string(op.Status)}
	// Decode only declared child metadata. Never ingest arbitrary State, task,
	// settings, endpoints, bindings, credentials or tool argument fields.
	if op.Type == operation.TypeRemoteJob {
		var state struct {
			Plan struct {
				Type string
				Data struct {
					ChildID, Action string
					Configuration   *struct{ Runtime jsontext.Value }
				}
			}
			Handle struct {
				ChildID string
				Finish  *struct{ Result struct{ Status string } }
			}
		}
		if json.Unmarshal(op.State, &state) == nil && state.Plan.Type == "subagent" {
			v.ChildID = state.Handle.ChildID
			if v.ChildID == "" {
				v.ChildID = state.Plan.Data.ChildID
			}
			if state.Plan.Data.Configuration != nil {
				if s := sessionstore.SelectionFromConfiguration(state.Plan.Data.Configuration.Runtime); s != nil {
					v.Provider, v.Model, v.Effort = s.Provider, s.Model, string(s.Effort)
				}
			}
			if state.Handle.Finish != nil {
				switch state.Handle.Finish.Result.Status {
				case "completed", "failed":
					v.Outcome = state.Handle.Finish.Result.Status
				}
			}
		}
	}
	e.ops[v.ID] = v
}

// State contains only current operational metadata; sequence/time never enter a prompt.
func (e *Engine) State() State {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.state
	s.ThroughSequence = e.through
	s.Constraints.AllowedTools = slices.Clone(s.Constraints.AllowedTools)
	for _, op := range e.ops {
		if op.Status == "ready" || op.Status == "awaiting" || op.Status == "canceling" {
			s.Operations = append(s.Operations, op)
			s.ActiveOperations++
		}
	}
	slices.SortFunc(s.Operations, func(a, b OperationState) int { return cmp.Compare(a.ID, b.ID) })
	return s
}

func (e *Engine) Units() []Unit {
	e.mu.Lock()
	defer e.mu.Unlock()
	result := make([]Unit, 0, len(e.order))
	for _, id := range e.order {
		u := e.units[id]
		u.Item = clonePublicItem(u.Item)
		result = append(result, u)
	}
	return result
}

func (e *Engine) ResolveUnit(id string) (Unit, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	u, ok := e.units[id]
	if !ok || u.Class == Omit {
		return Unit{}, false
	}
	u.Item = clonePublicItem(u.Item)
	return u, true
}

func clonePublicItem(item llm.Item) llm.Item {
	if v, ok := item.Data.(llm.ToolResult); ok {
		v.Output = slices.Clone(v.Output)
		item.Data = v
	}
	return item
}

package contextengine

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"slices"
	"sync"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

type Engine struct {
	mu                                sync.Mutex
	config                            Config
	retriever                         Retriever
	selector                          Selector
	units                             map[string]Unit
	order, staged                     []string
	sources                           map[string][]string
	groups                            map[string][]string
	parts                             map[string]int
	serial                            uint64
	through                           sessionstore.Sequence
	state                             State
	ops                               map[string]OperationState
	checkpoint                        Checkpoint
	checkpointRefs                    []HistoryRef
	failures                          []string
	last                              *Diagnostics
	excluded                          int
	cache                             string
	toolsSupported                    func(string) bool
	taskSource, taskUnit, requestUnit string
}

func New(c Config, r Retriever, s Selector) (*Engine, error) {
	c, err := c.Resolve()
	if err != nil {
		return nil, err
	}
	if r == nil {
		r = NewLexical()
	}
	if s == nil {
		s = DeterministicSelector{}
	}
	return &Engine{config: c, retriever: r, selector: s, units: map[string]Unit{}, sources: map[string][]string{}, groups: map[string][]string{}, parts: map[string]int{}, ops: map[string]OperationState{}, checkpoint: Checkpoint{Version: Version}, cache: "memory"}, nil
}
func (e *Engine) SetCapabilities(tools func(string) bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.toolsSupported = tools
}

// SetTaskSource binds an explicit delegated task input, never a guessed intent
// from conversation. The child factory obtains it from canonical ChildConfig.
func (e *Engine) SetTaskSource(key string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.taskSource = key
}

func (e *Engine) Configure(r Runtime, constraints Constraints, instructions bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.state.Runtime, e.state.Constraints, e.state.ProjectInstructionsBound = r, constraints, instructions
	e.state.Constraints.AllowedTools = slices.Clone(constraints.AllowedTools)
	if e.state.ProjectInstructions != nil {
		e.state.ProjectInstructionsBound = true
	}
}

func (e *Engine) Add(u Unit, sourceKey string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.serial++
	u.ID = fmtID(e.serial)
	if u.Kind == UserMessage && u.Required && !u.RetrievalOnly {
		if prior, ok := e.units[e.requestUnit]; ok && !prior.Staged && prior.ID != e.taskUnit {
			prior.Required = false
			if prior.Class == Pin {
				prior.Class = Keep
			}
			e.units[prior.ID] = prior
		}
		e.requestUnit = u.ID
	}
	if e.taskSource != "" && sourceKey == e.taskSource && !u.RetrievalOnly && e.taskUnit == "" {
		e.taskUnit = u.ID
		u.Class, u.Required = Pin, true
	}
	public, ok := PublicItem(u.Item)
	if u.Class == Omit {
		ok = false
	}
	if !ok {
		e.excluded++
		u.Class = Omit
		u.Item = llm.Item{}
	} else {
		u.Item = public
	}
	// Cache the worst public rendering cost once per new unit. Allow for a
	// larger live-assigned sequence in retrieval provenance (uint64 <= 20 digits).
	u.Tokens = max(Estimate(u.Item), Estimate(Portable(u, true))) + 32
	hash := sha256.Sum256([]byte(string(u.Kind) + "\x00" + Text(u.Item)))
	u.Fingerprint = hex.EncodeToString(hash[:])
	e.units[u.ID] = u
	if u.ParentID != "" && u.Class != Omit {
		e.parts[u.ParentID]++
	}
	if u.Kind == ErrorUnit || u.Class == Pin && u.Kind == ToolResult {
		e.failures = append(e.failures, u.ID)
	}
	if u.Staged {
		e.staged = append(e.staged, u.ID)
	} else if !u.RetrievalOnly && u.Class != Omit {
		e.order = append(e.order, u.ID)
	}
	e.sources[sourceKey] = append(e.sources[sourceKey], u.ID)
	if u.Group != "" {
		e.groups[u.Group] = append(e.groups[u.Group], u.ID)
	}
	e.retriever.Put(u)
	return u.ID
}

func fmtID(n uint64) string {
	const digits = "0123456789"
	var b [20]byte
	for i := 19; i >= 0; i-- {
		b[i] = digits[n%10]
		n /= 10
	}
	return "unit-" + string(b[:])
}

func (e *Engine) RemoveStaged(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if u, ok := e.units[id]; ok && u.Staged {
		if u.Class == Omit {
			e.excluded--
		}
		if u.ParentID != "" && u.Class != Omit {
			e.parts[u.ParentID]--
		}
		delete(e.units, id)
		e.retriever.Remove(id)
		e.staged = slices.DeleteFunc(e.staged, func(s string) bool { return s == id })
	}
}

func (e *Engine) Commit(turn string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, id := range e.staged {
		u := e.units[id]
		u.Staged = false
		u.Required = id == e.taskUnit || id == e.requestUnit
		if u.Class == Pin && u.Kind != ErrorUnit && !u.Required {
			u.Class = Keep
		}
		if u.Source.TurnID == "" {
			u.Source.TurnID = turn
		}
		e.units[id] = u
	}
	for _, id := range e.staged {
		u := e.units[id]
		if !u.RetrievalOnly && u.Class != Omit {
			e.order = append(e.order, id)
		}
	}
	e.staged = nil
}

// Observe connects projection units to the actual durable sequence. A live
// coordinator projects before append; replay already carries these identities.
func (e *Engine) Observe(item sessionstore.Item, sourceKey string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, id := range e.sources[sourceKey] {
		u, ok := e.units[id]
		if !ok {
			continue
		}
		u.Source.Sequence = item.Sequence
		u.Source.RecordedAt = item.RecordedAt
		e.units[id] = u
		if u.Class != Omit {
			e.checkpointRefs = append(e.checkpointRefs, u.Source)
		}
	}
	delete(e.sources, sourceKey)
	if item.Sequence > e.through {
		e.through = item.Sequence
	}
	if r, ok := item.Data.(sessionstore.HostRecord); ok {
		if r.Kind == sessionstore.HostProjectInstructions {
			e.state.ProjectInstructionsBound = true
			m := r.ProjectInstructions.Metadata()
			e.state.ProjectInstructions = &m
		}
		if r.Kind == "configuration" {
			if s := sessionstore.SelectionFromConfiguration(r.Configuration); s != nil {
				e.state.Runtime = runtimeFromSelection(*s, e.state.Runtime.Tools)
			}
		}
		if r.Kind == sessionstore.HostRuntimeApplied && r.Selection != nil {
			e.state.Runtime = runtimeFromSelection(*r.Selection, e.state.Runtime.Tools)
		}
	}
	if status, ok := item.Data.(sessionstore.ToolCallStatus); ok {
		for _, op := range status.Operations {
			e.observeOperation(op)
		}
	}
	if e.toolsSupported != nil {
		e.state.Runtime.Tools = e.toolsSupported(e.state.Runtime.Provider)
	}
	if int(e.through-e.checkpoint.ThroughSequence) >= e.config.CheckpointThreshold {
		e.checkpoint = Checkpoint{Version: Version, FromSequence: e.checkpoint.ThroughSequence + 1, ThroughSequence: e.through, State: e.snapshotState(), CreatedAt: item.RecordedAt}
		// Incremental range references avoid rescanning old bodies at checkpoints.
		e.checkpoint.SourceRefs = e.checkpointRefs
		e.checkpointRefs = nil
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

func (e *Engine) snapshotState() State {
	s := e.state
	s.ThroughSequence = e.through
	s.CheckpointBoundary = e.checkpoint.ThroughSequence
	if u, ok := e.units[e.taskUnit]; ok && u.Class != Omit {
		ref := u.Source
		s.Task = &ref
	}
	if u, ok := e.units[e.requestUnit]; ok && u.Class != Omit {
		ref := u.Source
		s.CurrentRequest = &ref
	}
	if s.ProjectInstructions != nil {
		m := *s.ProjectInstructions
		s.ProjectInstructions = &m
	}
	s.Constraints.AllowedTools = slices.Clone(s.Constraints.AllowedTools)
	var active, failed, children []OperationState
	for _, op := range e.ops {
		switch op.Status {
		case "ready", "awaiting", "canceling":
			s.ActiveOperations++
			active = append(active, op)
		case "failed":
			s.FailedOperations++
			failed = append(failed, op)
		}
		if op.ChildID != "" {
			s.ObservedChildren++
			if op.Status == "completed" || op.Status == "canceled" {
				children = append(children, op)
			}
		}
	}
	for _, list := range [][]OperationState{active, failed, children} {
		slices.SortFunc(list, func(a, b OperationState) int { return cmp.Compare(a.ID, b.ID) })
	}
	// Exact counts plus source IDs replace unbounded older operation metadata.
	// This never claims unobserved child activity or invents resolved failures.
	s.Operations = append(s.Operations, active[:min(32, len(active))]...)
	s.Operations = append(s.Operations, failed[:min(16, len(failed))]...)
	s.Operations = append(s.Operations, children[:min(8, len(children))]...)
	s.ReferencedOperationStates = len(active) + len(failed) + len(children) - len(s.Operations)
	return s
}

func (e *Engine) State() State { e.mu.Lock(); defer e.mu.Unlock(); return e.snapshotState() }

type BuildInput struct {
	Runtime          Runtime
	Instructions     llm.Item
	SchemaTokens     int64
	TransportReserve int64
	// Cost includes renderer framing and any same-provider transient replay.
	Cost func(Unit, string, bool) int64
}

func (e *Engine) Build(in BuildInput) (Package, Diagnostics, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	state := e.snapshotState()
	if in.Runtime.Provider != "" {
		state.Runtime = in.Runtime
	}
	budget, err := e.config.budget(state.Runtime, in.SchemaTokens)
	if err != nil {
		return Package{}, Diagnostics{}, err
	}
	if in.TransportReserve < 0 || in.TransportReserve >= budget.Input {
		return Package{}, Diagnostics{}, &Error{"required_context_exceeds_budget"}
	}
	available := budget.Input - in.TransportReserve
	instructions, ok := PublicItem(in.Instructions)
	if !ok {
		return Package{}, Diagnostics{}, &Error{"sensitive_instructions"}
	}
	if in.Cost == nil {
		in.Cost = func(u Unit, reason string, ref bool) int64 {
			if ref {
				return Estimate(llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: ReferenceText(u)}})
			}
			return Estimate(Portable(u, reason == "retrieved"))
		}
	}
	// Counts are exact; optional operation detail is bounded before recent/raw
	// selection so it cannot crowd out required instructions and current input.
	mandatory := int64(0)
	var persistent []Unit
	for i, id := range []string{e.requestUnit, e.taskUnit} {
		if i == 1 && id == e.requestUnit {
			continue
		}
		if u, ok := e.units[id]; ok && !u.Staged {
			if u.Class == Omit {
				return Package{}, Diagnostics{}, &Error{"sensitive_current_input"}
			}
			persistent = append(persistent, u)
			mandatory += in.Cost(u, "pin", false)
		}
	}
	for _, id := range e.staged {
		u := e.units[id]
		if u.Required && u.Class != Omit {
			mandatory += in.Cost(u, "pin", u.Class == Reference)
		}
	}
	state = budgetState(state, instructions, available-mandatory)
	base := Estimate(instructions) + Estimate(StateItem(state))
	if base+mandatory > available {
		return Package{}, Diagnostics{}, &Error{"required_context_exceeds_budget"}
	}
	var pinned, recent []Unit
	for _, id := range e.staged {
		u := e.units[id]
		if u.Class == Omit && u.Required {
			return Package{}, Diagnostics{}, &Error{"sensitive_current_input"}
		}
		if u.Class != Omit && !u.RetrievalOnly {
			pinned = append(pinned, u)
		}
	}
	pinned = append(pinned, persistent...)
	// Candidate construction visits only a bounded recent suffix, not old bodies.
	weight := int64(0)
	for i := len(e.order) - 1; i >= 0; i-- {
		u := e.units[e.order[i]]
		if u.Class == Omit || u.RetrievalOnly {
			continue
		}
		recent = append(recent, u)
		weight += min(u.Tokens, e.config.LargeToolTokens)
		if weight > available*2 {
			break
		}
	}
	// Recent failed receipts precede ordinary recent material. This is machine
	// failure evidence, not a semantic conclusion about user decisions.
	for i := len(e.failures) - 1; i >= 0; i-- {
		u, exists := e.units[e.failures[i]]
		if !exists || u.Class == Omit {
			continue
		}
		if op, known := e.ops[u.Source.OperationID]; known && op.Status != "failed" {
			continue
		}
		pinned = append(pinned, u)
		if len(pinned) > 32+len(e.staged) {
			break
		}
	}
	// Search the current explicit request, not a concatenation of unrelated
	// recent questions. The delegated task is a fallback for autonomous loops.
	query := ""
	if u, ok := e.units[e.requestUnit]; ok {
		query = Text(u.Item)
	}
	if query == "" {
		if u, ok := e.units[e.taskUnit]; ok {
			query = Text(u.Item)
		}
	}
	var retrieval RetrievalDiagnostics
	elapsed := int64(0)
	selection := SelectionInput{Pinned: pinned, Recent: recent, Budget: available - base, RecentReserve: e.config.RecentReserve, Cost: in.Cost}
	if query != "" {
		selection.RetrievalReserve = e.config.RetrievalReserve
		selection.Retrieve = func(retained []Selected, remaining int64) []Unit {
			start := time.Now()
			currentTurn := e.units[e.requestUnit].Source.TurnID
			// Eligibility is evaluated before top-K. Only actually retained raw
			// sources suppress retrieval; a recent *candidate* may still be absent.
			hits := e.retriever.Search(query, len(e.units), func(id string) bool {
				u, ok := e.units[id]
				root := u.ID
				if u.ParentID != "" {
					root = u.ParentID
				}
				if !ok || u.Class == Omit || u.Staged || root == e.requestUnit || root == e.taskUnit || currentTurn != "" && u.Source.TurnID == currentTurn || u.Class == Reference && e.parts[u.ID] > 0 {
					retrieval.SkippedIneligible++
					return false
				}
				if overlapsSelected(u, retained) {
					retrieval.SkippedRecent++
					return false
				}
				return true
			})
			retrieval.Candidates = len(hits)
			chosen := slices.Clone(retained)
			var result []Unit
			for _, h := range hits {
				u := e.units[h.ID]
				if overlapsSelected(u, chosen) {
					retrieval.SkippedDuplicate++
					continue
				}
				cost := in.Cost(u, "retrieved", false)
				if cost > remaining {
					retrieval.SkippedBudget++
					continue
				}
				remaining -= cost
				result = append(result, u)
				chosen = append(chosen, Selected{Unit: u, Reason: "retrieved"})
				if len(result) >= e.config.RetrievalLimit {
					break
				}
			}
			elapsed = time.Since(start).Microseconds()
			return result
		}
	}
	selected, err := e.selector.Select(selection)
	if err != nil {
		return Package{}, Diagnostics{}, err
	}
	// Native call/result groups are atomic. Add their partners if they fit;
	// otherwise use portable history for that group, avoiding orphan native calls.
	selected = closeGroups(selected, e.groups, e.units, available-base, in.Cost)
	selected = deduplicateRanges(selected)
	order := map[string]int{}
	// Selected IDs have a monotonic insertion identity; staged items sort last.
	for _, s := range selected {
		if s.Unit.Staged {
			order[s.Unit.ID] = 1
		}
	}
	slices.SortFunc(selected, func(a, b Selected) int {
		if n := cmp.Compare(order[a.Unit.ID], order[b.Unit.ID]); n != 0 {
			return n
		}
		return cmp.Compare(a.Unit.ID, b.Unit.ID)
	})
	p := Package{Version: Version, Runtime: state.Runtime, Instructions: instructions, State: state, Checkpoint: cloneCheckpoint(e.checkpoint), Selected: selected, Budget: budget, EstimatedTokens: base}
	d := Diagnostics{Version: Version, Provider: state.Runtime.Provider, Model: state.Runtime.Model, Effort: state.Runtime.Effort, RuntimeRevision: state.Runtime.Revision, ThroughSequence: e.through, CheckpointBoundary: e.checkpoint.ThroughSequence, CheckpointVersion: Version, CanonicalUnits: len(e.units), Excluded: e.excluded, Budget: budget, RetrievalMicros: elapsed, Measurement: "estimated", Cache: e.cache}
	for _, s := range selected {
		p.EstimatedTokens += in.Cost(s.Unit, s.Reason, s.ReferenceOnly)
		if s.ReferenceOnly {
			p.ReferencedUnitIDs = append(p.ReferencedUnitIDs, s.Unit.ID)
			d.Referenced++
		} else {
			p.IncludedUnitIDs = append(p.IncludedUnitIDs, s.Unit.ID)
		}
		if s.Reason == "retrieved" {
			d.Retrieved++
		} else if !s.ReferenceOnly {
			d.Recent++
		}
	}
	for i := range p.Selected {
		item := p.Selected[i].Unit.Item
		if p.Selected[i].ReferenceOnly {
			if v, ok := item.Data.(llm.ToolResult); ok {
				item.Data = llm.ToolResult{CallID: v.CallID}
			}
		}
		p.Selected[i].Unit.Item = clonePublicItem(item)
	}
	retrieval.Selected = d.Retrieved
	d.Retrieval = retrieval
	d.Omitted = d.CanonicalUnits - len(selected) - d.Excluded
	d.TransportReserve = in.TransportReserve
	d.EstimatedInputTokens = p.EstimatedTokens + in.TransportReserve
	d.Utilization = 100 * float64(d.EstimatedInputTokens) / float64(budget.Input)
	e.last = &d
	return p, d, nil
}

// ResolveUnit returns original eligible evidence from the derived generation.
// If this index was discarded, its HistoryRef can be reprojected from Store.
// It never returns reasoning, provider replay IDs or an excluded input.
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

func StateItem(s State) llm.Item {
	b, _ := json.Marshal(s)
	return llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleSystem, Text: "Current Unreal machine state (derived from canonical records; not a conversation summary):\n" + string(b)}}
}
func runtimeFromSelection(s sessionstore.RuntimeSelection, tools bool) Runtime {
	return Runtime{Provider: s.Provider, Model: s.Model, Effort: string(s.Effort), Revision: s.Revision, ContextWindow: s.ContextWindow, Tools: tools}
}
func contentKey(u Unit) string { return u.Fingerprint }
func deduplicateRanges(selected []Selected) []Selected {
	whole := map[string]bool{}
	for _, s := range selected {
		if s.Unit.ParentID == "" && !s.ReferenceOnly {
			whole[s.Unit.ID] = true
		}
	}
	var kept []Selected
	for _, s := range selected {
		if s.Unit.ParentID != "" && whole[s.Unit.ParentID] {
			continue
		}
		overlaps := false
		if s.Unit.ParentID != "" {
			for _, prior := range kept {
				if prior.Unit.ParentID == s.Unit.ParentID && prior.Unit.Source.OutputIndex == s.Unit.Source.OutputIndex && prior.Unit.Source.StartByte < s.Unit.Source.EndByte && s.Unit.Source.StartByte < prior.Unit.Source.EndByte {
					overlaps = true
					break
				}
			}
		}
		if !overlaps {
			kept = append(kept, s)
		}
	}
	return kept
}
func cloneCheckpoint(c Checkpoint) Checkpoint {
	c.SourceRefs = slices.Clone(c.SourceRefs)
	c.State.Operations = slices.Clone(c.State.Operations)
	c.State.Constraints.AllowedTools = slices.Clone(c.State.Constraints.AllowedTools)
	if c.State.ProjectInstructions != nil {
		m := *c.State.ProjectInstructions
		c.State.ProjectInstructions = &m
	}
	if c.State.Task != nil {
		r := *c.State.Task
		c.State.Task = &r
	}
	if c.State.CurrentRequest != nil {
		r := *c.State.CurrentRequest
		c.State.CurrentRequest = &r
	}
	return c
}

func budgetState(s State, instructions llm.Item, available int64) State {
	ops := s.Operations
	s.Operations = nil
	s.ReferencedOperationStates += len(ops)
	minimum := Estimate(instructions) + Estimate(StateItem(s))
	allowance := min(int64(4096), max(int64(0), available-minimum)/3)
	for _, op := range ops {
		candidate := s
		candidate.Operations = append(slices.Clone(s.Operations), op)
		candidate.ReferencedOperationStates--
		if Estimate(instructions)+Estimate(StateItem(candidate)) > minimum+allowance {
			break
		}
		s = candidate
	}
	return s
}

func closeGroups(selected []Selected, groups map[string][]string, units map[string]Unit, budget int64, cost func(Unit, string, bool) int64) []Selected {
	seen := map[string]bool{}
	used := int64(0)
	for _, s := range selected {
		seen[s.Unit.ID] = true
		used += cost(s.Unit, s.Reason, s.ReferenceOnly)
	}
	for _, s := range slices.Clone(selected) {
		if s.Unit.Group == "" || s.Reason == "retrieved" {
			continue
		}
		var partners []Unit
		extra := int64(0)
		for _, id := range groups[s.Unit.Group] {
			u, exists := units[id]
			if exists && !seen[id] && u.Class != Omit {
				partners = append(partners, u)
				extra += cost(u, "recent", u.Class == Reference)
			}
		}
		if used+extra <= budget {
			for _, u := range partners {
				seen[u.ID] = true
				selected = append(selected, Selected{u, "recent", u.Class == Reference})
			}
			used += extra
		} else {
			for i := range selected {
				if selected[i].Unit.Group == s.Unit.Group {
					selected[i].Reason = "recent-portable"
				}
			}
		}
	}
	return selected
}

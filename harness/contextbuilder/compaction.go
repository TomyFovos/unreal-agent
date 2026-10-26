package contextbuilder

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

type nativeReplay struct {
	origin, providerID string
	extra              []llm.Item
	after              []llm.Item
	only               bool
	charge             int64
}

// EnableCompaction changes the request projection only. Existing canonical
// replay and the comparison Builder continue to use identical source records.
func EnableCompaction(b Builder, config contextengine.Config, constraints contextengine.Constraints) (*contextengine.Engine, error) {
	c, err := config.Resolve()
	if err != nil {
		return nil, err
	}
	if c.Mode == "legacy" {
		return nil, nil
	}
	concrete, ok := b.(*builder)
	if !ok {
		return nil, &contextengine.Error{Code: "unsupported_builder"}
	}
	e, err := contextengine.New(c, nil, nil)
	if err != nil {
		return nil, err
	}
	concrete.engine = e
	concrete.contextConfig = c
	concrete.constraints = constraints
	concrete.native = map[string]nativeReplay{}
	concrete.runningUnits = map[string][]string{}
	concrete.blockedCalls = map[string]bool{}
	concrete.callGroups = map[string]string{}
	concrete.continuations = map[string]*continuationGroup{}
	concrete.turnBounds = map[session.TurnID]string{}
	return e, nil
}

func (b *builder) SetContextRuntime(s sessionstore.RuntimeSelection, tools bool) {
	b.contextRuntime = contextengine.Runtime{Provider: s.Provider, Model: s.Model, Effort: string(s.Effort), Revision: s.Revision, ContextWindow: s.ContextWindow, Tools: tools}
	if b.engine != nil {
		b.engine.Configure(b.contextRuntime, b.constraints, b.instructions != "")
	}
}

// Coordinator-owned projection only. Human controls never mutate the Builder;
// the next turn boundary supplies fresh, safe permission constraints.
func (b *builder) SetContextConstraints(c contextengine.Constraints) {
	c.AllowedTools = slices.Clone(c.AllowedTools)
	b.constraints = c
	if b.engine != nil {
		b.engine.Configure(b.contextRuntime, c, b.instructions != "")
	}
}

// SetTaskInput retains the child's explicit initial task across long tool loops.
// This is a rebuildable binding to an existing canonical input, not task memory.
func (b *builder) SetTaskInput(id inbox.ID) {
	if b.engine != nil && id != "" {
		b.engine.SetTaskSource("input:" + string(id))
	}
}

// SetHistoryItem is coordinator-local. ObserveHistory uses only the engine's
// locked derived metadata and can also receive durable Host control records.
func (b *builder) SetHistoryItem(item sessionstore.Item) {
	b.source = item
	b.sourceOutputIndex = 0
}
func (b *builder) ObserveHistory(item sessionstore.Item) {
	if b.engine != nil {
		b.engine.Observe(item, sourceKey(item))
	}
}
func (b *builder) ObserveOperation(op operation.Operation) {
	if b.engine != nil {
		b.engine.ObserveOperation(op)
	}
}

func sourceKey(item sessionstore.Item) string {
	switch v := item.Data.(type) {
	case inbox.Input:
		return "input:" + string(v.ID)
	case session.Turn:
		return "turn:" + string(v.ID)
	case sessionstore.ModelResponse:
		return "response:" + string(v.TurnID)
	case sessionstore.ToolCallStatus:
		return "receipt:" + string(v.TurnID) + ":" + v.CallID
	}
	return string(item.Kind) + ":" + fmt.Sprint(item.Sequence)
}

func (b *builder) sourceRef() contextengine.HistoryRef {
	r := contextengine.HistoryRef{SourceType: string(b.source.Kind), Sequence: b.source.Sequence, RecordedAt: b.source.RecordedAt}
	switch v := b.source.Data.(type) {
	case inbox.Input:
		if v.Kind == inbox.InputPeer {
			if peer, err := v.DecodePeerMessage(); err == nil {
				r.OperationID = peer.Handle
				if strings.HasPrefix(peer.Sender, "child-") {
					r.ChildID = peer.Sender
				}
			}
		}
	case sessionstore.ModelResponse:
		r.TurnID = string(v.TurnID)
		r.OutputIndex = b.sourceOutputIndex
	case sessionstore.ToolCallStatus:
		r.TurnID = string(v.TurnID)
		if len(v.Operations) > 0 {
			r.OperationID = string(v.Operations[0].ID)
		}
		for _, op := range v.Operations {
			if op.Status == operation.StatusFailed {
				r.OperationID = string(op.ID)
				break
			}
		}
	}
	return r
}

func (b *builder) addUnit(item llm.Item, kind contextengine.Kind, class contextengine.Class, staged, required bool, group string, extra []llm.Item) string {
	if b.engine == nil {
		return ""
	}
	u := contextengine.Unit{Kind: kind, Class: class, Source: b.sourceRef(), Origin: b.historyProvider, Group: group, Item: item, Staged: staged, Required: required}
	id := b.engine.Add(u, sourceKey(b.source))
	b.lastUnit = id
	charge := int64(0)
	for _, private := range extra {
		charge += contextengine.Estimate(private)
	}
	if item.ProviderID != "" {
		charge += contextengine.Estimate(item.ProviderID)
	}
	b.native[id] = nativeReplay{origin: b.historyProvider, providerID: item.ProviderID, extra: slices.Clone(extra), charge: charge}
	if message, ok := item.Data.(llm.Message); ok && len(message.Text) > 2048 {
		if _, eligible := contextengine.PublicItem(item); eligible {
			for _, span := range textSpans(message.Text) {
				r := b.sourceRef()
				r.StartByte, r.EndByte = span[0], span[1]
				part := contextengine.Unit{Kind: kind, Class: contextengine.Keep, Source: r, ParentID: id, Origin: b.historyProvider, Item: llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: message.Role, Text: message.Text[span[0]:span[1]], Phase: message.Phase}}, Staged: staged, RetrievalOnly: true}
				if kind == contextengine.ErrorUnit && (span[0] == 0 || span[1] == len(message.Text)) {
					part.Class = contextengine.Pin
					part.RetrievalOnly = false
				}
				b.engine.Add(part, sourceKey(b.source))
			}
		}
	}
	return id
}

// Overlap keeps bounded filenames, symbols and error codes searchable across a
// chunk boundary. Every returned byte range is an exact UTF-8 source substring.
func textSpans(text string) [][2]int {
	var spans [][2]int
	for start := 0; start < len(text); {
		end := min(start+1024, len(text))
		for end < len(text) && !utf8.RuneStart(text[end]) {
			end--
		}
		spans = append(spans, [2]int{start, end})
		if end == len(text) {
			break
		}
		start = end - 128
		for start > 0 && !utf8.RuneStart(text[start]) {
			start--
		}
	}
	return spans
}

func (b *builder) contextResponse(response llm.Response) {
	if b.engine == nil {
		return
	}
	b.acknowledgeContinuation(response)
	b.responseSerial++
	groupID := fmt.Sprintf("response:%s:%d", b.historyProvider, b.responseSerial)
	group := &continuationGroup{calls: map[string]*continuationCall{}}
	b.continuations[groupID] = group
	var private []llm.Item
	last := ""
	for index, item := range response.Output {
		b.sourceOutputIndex = index
		if item.Type == llm.ItemReasoning {
			private = append(private, item)
			continue
		}
		kind := contextengine.ModelMessage
		switch v := item.Data.(type) {
		case llm.Message:
		case llm.ToolCall:
			kind = contextengine.ToolRequest
			b.callGroups[v.CallID] = groupID
			group.calls[v.CallID] = &continuationCall{}
			group.chain = true
			_, eligible := contextengine.PublicItem(item)
			b.blockedCalls[v.CallID] = !eligible
		default:
			continue
		}
		if _, safe := contextengine.PublicItem(item); !safe {
			// Preserve eligibility accounting without retaining the original body
			// in the public index or adding an OMIT member to a native group.
			b.addUnit(item, kind, contextengine.Omit, false, false, "", nil)
			// Never send redacted arguments under the original native provider ID.
			group.portable = true
			item = llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "[Prior Unreal response content redacted; sensitive data omitted.]"}}
		}
		last = b.addUnit(item, kind, contextengine.Keep, false, false, groupID, private)
		private = nil
	}
	if len(private) > 0 {
		if last == "" {
			last = b.addUnit(llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant}}, contextengine.ModelMessage, contextengine.Keep, false, false, groupID, nil)
			replay := b.native[last]
			replay.only = true
			b.native[last] = replay
		}
		replay := b.native[last]
		replay.after = private
		for _, item := range private {
			replay.charge += contextengine.Estimate(item)
		}
		b.native[last] = replay
	}
	if response.Failure != nil {
		b.sourceOutputIndex = len(response.Output)
		b.addUnit(llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: response.Failure.Message}}, contextengine.ErrorUnit, contextengine.Pin, false, false, "", nil)
	}
}

func (b *builder) contextReceipt(callID string, output []llm.ToolResultOutput, running bool, state llm.ToolResultState) {
	if b.engine == nil {
		return
	}
	for _, id := range b.runningUnits[callID] {
		b.engine.RemoveStaged(id)
	}
	delete(b.runningUnits, callID)
	item := llm.Item{Type: llm.ItemToolResult, Data: llm.ToolResult{CallID: callID, Output: output, State: state}}
	if _, safe := contextengine.PublicItem(item); !safe || b.blockedCalls[callID] {
		b.addUnit(item, contextengine.ToolResult, contextengine.Omit, false, false, "", nil)
		if stub, present := b.redactedReceipt(running); present {
			output = stub
			item.Data = llm.ToolResult{CallID: callID, Output: output, State: state}
		} else {
			// No canonical receipt means no claim of execution/completion.
			output = stub
			item.Data = llm.ToolResult{CallID: callID, Output: stub}
			if group := b.continuations[b.callGroups[callID]]; group != nil {
				group.portable = true
			}
		}
	}
	class := contextengine.Keep
	failed := false
	if status, ok := b.source.Data.(sessionstore.ToolCallStatus); ok {
		failed = status.Status.Error != ""
		for _, op := range status.Operations {
			if op.Status == operation.StatusFailed {
				failed = true
			}
		}
	}
	large := contextengine.Estimate(item) > b.contextConfig.LargeToolTokens
	image := false
	for _, o := range output {
		if o.Kind == llm.ToolResultImage {
			image = true
		}
	}
	if large {
		class = contextengine.Reference
	} else if failed {
		class = contextengine.Pin
	}
	// A current native image must reach its provider intact or fail budget
	// validation. Never silently turn a requested image into a textual receipt.
	if image {
		class = contextengine.Keep
		large = false
	}
	group := b.callGroups[callID]
	if group == "" {
		group = b.callOrigins[callID] + ":" + b.sourceRef().TurnID + ":" + callID
	}
	id := b.addUnit(item, contextengine.ToolResult, class, true, image && !b.textOnly, group, nil)
	if replayGroup := b.continuations[group]; replayGroup != nil {
		if call := replayGroup.calls[callID]; call != nil {
			call.receipt, call.terminal, call.received = id, !running, false
		}
	}
	replay := b.native[id]
	replay.origin = b.callOrigins[callID]
	b.native[id] = replay
	if running {
		b.runningUnits[callID] = append(b.runningUnits[callID], id)
	}
	if !large || running || class == contextengine.Omit || contextengine.Sensitive(contextengine.Text(item)) {
		return
	}
	// Large text receipts remain exact, retrievable chunks with byte provenance.
	// Their main unit uses a reference in recent/native conversation.
	for index, o := range output {
		if o.Kind != llm.ToolResultText {
			continue
		}
		for _, span := range textSpans(o.Value) {
			start, end := span[0], span[1]
			r := b.sourceRef()
			r.OutputIndex = index
			r.StartByte = start
			r.EndByte = end
			u := contextengine.Unit{Kind: contextengine.ToolResult, Class: contextengine.Keep, Source: r, ParentID: id, Origin: b.historyProvider, Item: llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: o.Value[start:end]}}, Staged: true, RetrievalOnly: true}
			if failed && (start == 0 || end == len(o.Value)) {
				u.Kind = contextengine.ErrorUnit
				u.Class = contextengine.Pin
				u.RetrievalOnly = false
			}
			b.engine.Add(u, sourceKey(b.source))
		}
	}
}

func (b *builder) renderSelected(s contextengine.Selected) []llm.Item {
	return b.renderContinuation(s, nil)
}

func (b *builder) renderContinuation(s contextengine.Selected, quoted map[string]bool) []llm.Item {
	u := s.Unit
	replay := b.native[u.ID]
	portable := quoted[u.Group] || b.textOnly || replay.origin != b.provider || s.Reason == "retrieved" || s.Reason == "recent-portable" || u.RetrievalOnly
	if group := b.continuations[u.Group]; group != nil {
		portable = portable || group.portable
	}
	if portable && replay.only {
		return nil // private-only reasoning has no provider-neutral rendering
	}
	if s.ReferenceOnly {
		text := contextengine.ReferenceText(u)
		if !portable {
			if receipt, ok := u.Item.Data.(llm.ToolResult); ok {
				return []llm.Item{{Type: llm.ItemToolResult, Data: llm.ToolResult{CallID: receipt.CallID, Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: text}}, State: receipt.State}}}
			}
		}
		return []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: text}}}
	}
	if portable {
		return []llm.Item{contextengine.Portable(u, s.Reason == "retrieved")}
	}
	result := slices.Clone(replay.extra)
	if !replay.only {
		item := u.Item
		item.ProviderID = replay.providerID
		result = append(result, item)
	}
	return append(result, replay.after...)
}

func (b *builder) buildCompacted() (Result, error) {
	return b.buildCompactedReserve(0)
}

// BuildWithReserve accounts for a provider's private framing/current receipt
// without persisting it as instructions, units, memory or canonical history.
func (b *builder) BuildWithReserve(reserve int64) (Result, error) {
	if reserve < 0 {
		return Result{}, &contextengine.Error{Code: "required_context_exceeds_budget"}
	}
	if b.conversation != nil && b.provider == "claude-native" {
		result, err := b.Build()
		if err != nil {
			return Result{}, err
		}
		budget, err := b.conversationBudget(result.Request.Tools)
		if err != nil {
			return Result{}, err
		}
		for _, item := range result.Request.Input {
			reserve += contextengine.Estimate(item)
			if reserve > budget {
				return Result{}, &llm.ContinuationError{Code: "epoch_context_limit"}
			}
		}
		return result, nil
	}
	if b.engine == nil {
		return b.Build()
	}
	return b.buildCompactedReserve(reserve)
}

func (b *builder) buildCompactedReserve(reserve int64) (Result, error) {
	return b.buildProjected(reserve, nil)
}

// BuildWithProjection uses the pinned adapter's actual final serializer, never
// its live client. The Coordinator owns this call just like BuildWithReserve.
func (b *builder) BuildWithProjection(projection llm.ContextProjection) (Result, error) {
	if projection.Measure == nil || projection.Reserve < 0 || projection.SchemaReserve < 0 {
		return Result{}, &contextengine.Error{Code: "invalid_projection_cost"}
	}
	if b.conversation != nil && b.provider == "claude-native" {
		result, err := b.Build()
		if err != nil {
			return Result{}, err
		}
		budget, err := b.conversationBudget(result.Request.Tools, projection.SchemaReserve)
		if err != nil {
			return Result{}, err
		}
		measured, err := projection.Measure(result.Request)
		if err != nil {
			return Result{}, err
		}
		if measured < 0 || projection.Reserve > budget || measured > budget-projection.Reserve {
			return Result{}, &llm.ContinuationError{Code: "epoch_context_limit"}
		}
		return result, nil
	}
	if b.engine == nil {
		return b.Build()
	}
	return b.buildProjected(projection.Reserve, &projection)
}

func (b *builder) buildProjected(reserve int64, projection *llm.ContextProjection) (Result, error) {
	runtime := b.contextRuntime
	runtime.Model = b.request.Model.ID
	runtime.Effort = string(b.request.Model.ReasoningEffort)
	runtime.Provider = b.provider
	runtime.Tools = len(b.request.Tools) > 0
	schema := int64(0)
	if len(b.request.Tools) > 0 {
		schema = contextengine.Estimate(b.request.Tools) + 512
	}
	if projection != nil {
		schema = max(schema, projection.SchemaReserve)
	}
	quoted := map[string]bool{}
	var projectionError error
	measure := func(items []llm.Item) (int64, error) {
		request := llm.Request{Model: b.request.Model, Tools: b.request.Tools, Input: items}
		request, err := llm.BindReceiptProjections(request)
		if err != nil {
			return 0, err
		}
		return projection.Measure(request)
	}
	// Same canonical call provides the result codec/name during marginal sizing.
	// Only eligible public partners are used; no private/unadopted body is copied.
	partners := map[string]llm.Item{}
	if projection != nil {
		for _, item := range append(slices.Clone(b.committedPrefix), b.stagedSuffix...) {
			if call, ok := item.Data.(llm.ToolCall); ok {
				if safe, eligible := contextengine.PublicItem(item); eligible {
					partners[call.CallID] = safe
				}
			}
		}
	}
	// Discarded at the end of this Build; portable retry gets a distinct key.
	var memo *projectionMemo
	if projection != nil {
		memo = newProjectionMemo(measure, len(partners))
	}
	cost := func(u contextengine.Unit, reason string, ref bool) int64 {
		if projection != nil {
			key := projectionKey(u, reason, ref, quoted[u.Group])
			if result, ok := u.Item.Data.(llm.ToolResult); ok {
				if call, exists := partners[result.CallID]; exists {
					key.partner = projectionPartnerKey{present: true, call: call.Data.(llm.ToolCall)}
				}
			}
			value, e := memo.cost(key, func() []llm.Item {
				return b.renderContinuation(contextengine.Selected{Unit: u, Reason: reason, ReferenceOnly: ref}, quoted)
			})
			if e == nil {
				return value
			}
			projectionError = e
			return 1 << 60
		}
		// Use a worst-case charge so changing an incomplete group to quoted
		// historical evidence cannot exceed the selector's budget.
		if ref {
			return contextengine.Estimate(llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: contextengine.ReferenceText(u)}}) + 128
		}
		if quoted[u.Group] {
			var charge int64
			for _, item := range b.renderContinuation(contextengine.Selected{Unit: u, Reason: "recent-portable"}, quoted) {
				charge += contextengine.Estimate(item)
			}
			return charge
		}
		charge := u.Tokens
		private := b.native[u.ID]
		if !b.textOnly && private.origin == b.provider {
			charge += private.charge
		}
		return charge
	}
	required, err := b.requiredContinuations()
	if err != nil {
		return Result{}, err
	}
	var baseCost func(llm.Item, contextengine.State) (int64, error)
	if projection != nil {
		baseCost = func(instructions llm.Item, state contextengine.State) (int64, error) {
			message := instructions.Data.(llm.Message)
			message.Text += "\n\n" + contextengine.StateItem(state).Data.(llm.Message).Text
			value, e := measure([]llm.Item{{Type: llm.ItemMessage, Data: message}})
			return value - reserve, e
		}
	}
	var measureSelected func(llm.Item, contextengine.State, []contextengine.Selected) (int64, error)
	if projection != nil {
		measureSelected = func(instructions llm.Item, state contextengine.State, selected []contextengine.Selected) (int64, error) {
			message := instructions.Data.(llm.Message)
			message.Text += "\n\n" + contextengine.StateItem(state).Data.(llm.Message).Text
			items := []llm.Item{{Type: llm.ItemMessage, Data: message}}
			for _, unit := range selected {
				items = append(items, b.renderContinuation(unit, quoted)...)
			}
			return measure(items)
		}
	}
	buildInput := contextengine.BuildInput{Runtime: runtime, Instructions: b.committedPrefix[0], SchemaTokens: schema, Cost: cost, BaseCost: baseCost, MeasureSelected: measureSelected, TransportReserve: reserve, RequiredGroups: required}
	p, d, err := b.engine.Build(buildInput)
	if projectionError != nil {
		return Result{}, projectionError
	}
	if contextengine.IsBudgetError(err) {
		// Private provider replay may exhaust the budget even when every public
		// receipt body fits. Retry a request-local, whole-group portable projection
		// before refusing. This drops no read body/revision and never changes the
		// canonical history, native arguments, or provider IDs in place.
		for group := range required {
			quoted[group] = true
		}
		p, d, err = b.engine.Build(buildInput)
		if projectionError != nil {
			return Result{}, projectionError
		}
	}
	if err != nil {
		return Result{}, err
	}
	request := b.request
	request.Tools = slices.Clone(request.Tools)
	instructions := b.committedPrefix[0].Data.(llm.Message)
	instructions.Text += "\n\n" + contextengine.StateItem(p.State).Data.(llm.Message).Text
	request.Input = []llm.Item{{Type: llm.ItemMessage, Data: instructions}}
	var changes []Change
	for _, selected := range p.Selected {
		request.Input = append(request.Input, b.renderContinuation(selected, quoted)...)
		if selected.ReferenceOnly {
			changes = append(changes, Change{Kind: ChangeOmitted, Source: selected.Unit.ID, Reason: "referenced"})
		}
	}
	// A final renderer check covers the actual JSON/framing estimate as well.
	actualEstimate := int64(0)
	if projection != nil {
		actualEstimate, err = measure(request.Input)
		if err != nil {
			return Result{}, err
		}
		d.EstimatedInputTokens = actualEstimate
		d.Utilization = 100 * float64(actualEstimate) / float64(p.Budget.Input)
		p.EstimatedTokens = actualEstimate - reserve
	} else {
		for _, item := range request.Input {
			actualEstimate += contextengine.Estimate(item)
		}
	}
	if actualEstimate > p.Budget.Input {
		return Result{}, &contextengine.Error{Code: "required_context_exceeds_budget"}
	}
	if err := llm.ValidateContinuation(request.Input); err != nil {
		return Result{}, err
	}
	request, err = llm.BindReceiptProjections(request)
	if err != nil {
		return Result{}, err
	}
	return Result{Request: request, Report: Report{Changes: changes, Context: &d}, Package: &p}, nil
}

func (b *builder) commitContext() {
	if b.engine == nil {
		return
	}
	var turn string
	if t, ok := b.source.Data.(session.Turn); ok {
		turn = string(t.ID)
	}
	if t, ok := b.source.Data.(session.Turn); ok {
		b.turnBounds[t.ID] = b.lastUnit
	}
	b.engine.Commit(turn)
}

// ContextDebug exposes selection reason codes only, never raw content.
func ContextDebug(result Result) []string {
	if result.Package == nil {
		return nil
	}
	var reasons []string
	for _, s := range result.Package.Selected {
		reasons = append(reasons, fmt.Sprintf("%s:%s", s.Unit.ID, strings.ReplaceAll(s.Reason, " ", "")))
	}
	return reasons
}

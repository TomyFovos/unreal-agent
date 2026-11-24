package contextbuilder

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"fmt"
	"slices"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	files "github.com/unreallabsai/unreal-agent/harness/native"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

type nativeReplay struct {
	origin, model string
	only          bool
	update        bool
	legacyNote    bool
}

// EnableWindow installs the three-layer projection over canonical history.
func EnableWindow(b Builder, config contextengine.Config, constraints contextengine.Constraints) (*contextengine.Engine, error) {
	c, err := config.Resolve()
	if err != nil {
		return nil, err
	}
	concrete, ok := b.(*builder)
	if !ok {
		return nil, &contextengine.Error{Code: "unsupported_builder"}
	}
	e, err := contextengine.New(c)
	if err != nil {
		return nil, err
	}
	concrete.derived = map[string][]llm.Item{}
	concrete.estimate = derivedEstimate{}
	concrete.outputLimits = map[string]int{}
	concrete.pendingReceipts = map[string]llm.ToolResult{}
	concrete.receiptSent = map[string]bool{}
	concrete.engine = e
	concrete.contextConfig = c
	concrete.constraints = constraints
	concrete.native = map[string]nativeReplay{}
	concrete.blockedCalls = map[string]bool{}
	concrete.blockedReasons = map[string]string{}
	concrete.callGroups = map[string]string{}
	concrete.continuations = map[string]*continuationGroup{}
	concrete.unsettledGroups = map[string]*continuationGroup{}
	concrete.unreadReceipts = nil
	concrete.turnBounds = map[session.TurnID]string{}
	concrete.turnEstimates = map[session.TurnID]requestEstimate{}
	concrete.receiptInfo = map[string]receiptInfo{}
	concrete.callInfo = map[string]receiptInfo{}
	return e, nil
}

func (b *builder) SetContextRuntime(s sessionstore.RuntimeSelection, tools bool) {
	if b.contextRuntime.Revision != s.Revision {
		b.estimate.prefixValid = false
	}
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

// SetHistoryItem is coordinator-local. ObserveHistory uses only the engine's
// locked derived metadata and can also receive durable Host control records.
func (b *builder) SetHistoryItem(item sessionstore.Item) {
	b.source = item
	b.sourceOutputIndex = 0
	if item.Kind == sessionstore.ItemFork {
		b.forkSequence = item.Sequence
		b.window = windowProjection{}
		b.windowTurn = windowTurn{}
		b.recordedContextPolicy = nil
		b.rebuildHistoricalWindow = false
		b.selectionContinuation = false
		clear(b.derived)
		b.estimate = derivedEstimate{}
	}
}
func (b *builder) ObserveHistory(item sessionstore.Item) {
	if b.engine != nil {
		b.engine.Observe(item, sourceKey(item))
	}
}
func (b *builder) ObserveReplayedHistory(item sessionstore.Item) {
	if b.engine != nil {
		b.engine.ObserveReplay(item)
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

func (b *builder) addUnit(item llm.Item, kind contextengine.Kind, class contextengine.Class, required bool, group string) string {
	id, _, _ := b.addUnitChecked(item, kind, class, required, group, b.sourceRef())
	return id
}

func (b *builder) addUnitChecked(item llm.Item, kind contextengine.Kind, class contextengine.Class, required bool, group string, ref contextengine.HistoryRef) (string, bool, bool) {
	if b.engine == nil {
		return "", false, false
	}
	u := contextengine.Unit{Kind: kind, Class: class, Source: ref, Origin: b.historyProvider, Group: group, Item: item, Required: required}
	id, eligible, redaction := b.engine.AddWithRedaction(u, sourceKey(b.source))
	if redaction != nil {
		if c, ok := item.Data.(llm.ToolCall); ok && !eligible {
			b.blockedReasons[c.CallID] = redaction.Reason
		}
		// Excluded receipt measurements include the replacement stub below.
		if _, receipt := item.Data.(llm.ToolResult); !receipt || class != contextengine.Omit {
			b.queueRedaction(id, redaction)
		}
	}
	b.lastUnit = id
	b.native[id] = nativeReplay{origin: b.historyProvider, model: b.historyModel}
	return id, eligible, redaction != nil
}

func (b *builder) contextResponse(response llm.Response) {
	limited := response.Stop == llm.StopMaxOutputTokens
	nestedOnly := len(response.Output) != 0 && !slices.ContainsFunc(response.Output, func(item llm.Item) bool { return !b.toolScopes.Nested(item) })
	if source, ok := b.source.Data.(sessionstore.ModelResponse); ok && !nestedOnly && source.WindowCache.Boundary == b.window.boundary {
		b.window.firstRequest = false
	}
	var acknowledgeWindow bool
	b.responseSerial++
	if !nestedOnly && !limited && response.Failure == nil {
		if source, ok := b.source.Data.(sessionstore.ModelResponse); ok && b.windowPrompt != nil && b.historyProvider == b.provider {
			_, acknowledgeWindow = b.turnBounds[source.TurnID]
		}
		b.acknowledgeContinuation(response)
		b.lastInput, b.lastInputKnown = 0, false
		b.lastEstimate = requestEstimate{}
		input, known := int64(0), false
		if response.Generation != nil {
			input, known = response.Generation.ContextInput()
		}
		if !known {
			reported := llm.ReportedTokens(response.Usage)
			input, known = reported.Input, reported.InputKnown
		}
		if known && b.historyProvider == b.provider && (b.historyModel == "" || b.historyModel == b.request.Model.ID) {
			b.lastInput, b.lastInputKnown = input, true
			if source, ok := b.source.Data.(sessionstore.ModelResponse); ok {
				if estimate, found := b.turnEstimates[source.TurnID]; found {
					b.lastEstimate = estimate
				}
			}
		}
	}
	if source, ok := b.source.Data.(sessionstore.ModelResponse); ok {
		delete(b.turnEstimates, source.TurnID)
		delete(b.turnBounds, source.TurnID)
	}
	groupID := fmt.Sprintf("response:%s:%d", b.historyProvider, b.responseSerial)
	group := &continuationGroup{}
	b.continuations[groupID] = group
	var private bool
	last := ""
	hasToolCall := false
	for index, item := range response.Output {
		b.sourceOutputIndex = index
		if b.toolScopes.Nested(item) {
			// Keep the unit positions referenced by older window and receipt
			// records, without a body or any model-facing continuation.
			kind := contextengine.ModelMessage
			if item.Type == llm.ItemToolCall {
				kind = contextengine.ToolRequest
			}
			b.addOmittedUnit(kind)
			if item.ParentCallID == "" {
				b.filteredScopes = true
			}
			continue
		}
		if call, ok := item.Data.(llm.ToolCall); ok {
			if call.Incomplete {
				continue
			}
			hasToolCall = true
			b.callOrigins[call.CallID] = b.historyProvider
			var args struct {
				MaxOutputLength int `json:"max_output_length"`
			}
			_ = json.Unmarshal([]byte(call.Arguments), &args)
			b.outputLimits[call.CallID] = args.MaxOutputLength
		}
		if item.Type == llm.ItemReasoning {
			if !item.OutputLimit {
				private = true
			}
			continue
		}
		omit := limited || item.OutputLimit
		kind := contextengine.ModelMessage
		switch v := item.Data.(type) {
		case llm.Message:
		case llm.ToolCall:
			kind = contextengine.ToolRequest
			b.callGroups[v.CallID] = groupID
			if group.calls == nil {
				group.calls = map[string]*continuationCall{}
				b.unsettledGroups[groupID] = group
			}
			if group.calls[v.CallID] == nil {
				group.calls[v.CallID] = &continuationCall{}
				group.pending++
			}
			// A completed call can already have a durable receipt. Send it
			// as historical data when its generating call is omitted.
			group.portable = group.portable || omit
		default:
			continue
		}
		id, safe, redacted := b.addUnitChecked(item, kind, contextengine.Keep, false, groupID, b.sourceRef())
		if safe && redacted {
			// Redacted messages/arguments cannot reuse the native response ID
			// or its reasoning. Replay the complete group as historical data.
			group.portable = true
		}
		if call, ok := item.Data.(llm.ToolCall); ok {
			b.blockedCalls[call.CallID] = !safe
		}
		if !safe {
			// The engine has already recorded the omitted source unit.
			// Private sources and unsupported payloads cannot be made public
			// by masking credential-shaped substrings in the original body.
			if omit {
				b.omitResponseUnit(id)
			}
			// Preserve eligibility accounting without retaining the original body
			// in the public index or adding an OMIT member to a native group.
			// Never send redacted arguments under the original native provider ID.
			group.portable = true
			item = llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "[Prior Unreal response content redacted; sensitive data omitted.]"}}
			id = b.addUnit(item, kind, contextengine.Keep, false, groupID)
		}
		if omit {
			b.omitResponseUnit(id)
		}
		last = id
		private = false
	}
	// Older logs can have a reasoning-only response. Preserve its unit
	// position for recorded cuts while keeping it out of the public window.
	if private && last == "" {
		id := b.addUnit(llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant}}, contextengine.ModelMessage, contextengine.Keep, false, groupID)
		if limited {
			b.omitResponseUnit(id)
		} else {
			replay := b.native[id]
			replay.only = true
			b.native[id] = replay
		}
	}
	if response.Failure != nil && !limited {
		b.sourceOutputIndex = len(response.Output)
		b.addUnit(llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: response.Failure.Message}}, contextengine.ErrorUnit, contextengine.Pin, false, "")
	}
	if acknowledgeWindow {
		b.window.deliveredThrough = b.lastUnit
	}
	if !nestedOnly && !limited && response.Failure == nil && !hasToolCall {
		if source, ok := b.source.Data.(sessionstore.ModelResponse); ok && source.TurnID == b.windowTurn.id {
			b.windowTurn = windowTurn{}
		}
	}
}

func (b *builder) addOmittedUnit(kind contextengine.Kind) string {
	b.lastUnit = b.engine.Add(contextengine.Unit{Kind: kind, Class: contextengine.Omit, Source: b.sourceRef(), Origin: b.historyProvider}, sourceKey(b.source))
	return b.lastUnit
}

func (b *builder) omitResponseUnit(id string) {
	b.engine.Omit(id)
	delete(b.native, id)
	b.filteredOutput = true
}

func (b *builder) contextReceipt(callID string, output []llm.ToolResultOutput, running bool, state llm.ToolResultState) {
	if b.toolScopes.Parent(callID) != "" {
		if !running {
			b.addOmittedUnit(contextengine.ToolResult)
		}
		return
	}
	if status, ok := b.source.Data.(sessionstore.ToolCallStatus); ok {
		info := toolReceiptInfo(status)
		info.bounded = b.receiptAlreadyBounded()
		b.callInfo[callID] = info
		for _, op := range status.Operations {
			if op.MaxOutputLength > 0 {
				b.outputLimits[callID] = op.MaxOutputLength
				break
			}
		}
	}
	result := llm.ToolResult{CallID: callID, Output: output, State: state}
	item := llm.Item{Type: llm.ItemToolResult, Data: result}
	if b.blockedCalls[callID] || !contextengine.PublicToolResultKinds(result) {
		reason := contextengine.NonPublic
		if b.blockedCalls[callID] {
			reason = b.blockedReasons[callID]
		}
		redaction := &llm.ContextRedaction{Reason: reason, CallID: callID, BeforeBytes: contextengine.PublicPayloadBytes(item)}
		id := b.addUnit(item, contextengine.ToolResult, contextengine.Omit, false, "")
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
		redaction.AfterBytes = contextengine.PublicPayloadBytes(item)
		b.queueRedaction(id, redaction)
	}
	if running {
		if !b.receiptSent[callID] {
			if _, exists := b.pendingReceipts[callID]; !exists {
				b.pendingOrder = append(b.pendingOrder, callID)
			}
			b.pendingReceipts[callID] = item.Data.(llm.ToolResult)
		}
		return
	}
	delete(b.pendingReceipts, callID)
	b.appendReceipt(item, false)
}

func (b *builder) appendReceipt(item llm.Item, running bool) string {
	callID := item.Data.(llm.ToolResult).CallID
	if b.toolScopes.Parent(callID) != "" {
		return b.addOmittedUnit(contextengine.ToolResult)
	}
	group := b.callGroups[callID]
	if group == "" {
		group = b.callOrigins[callID] + ":" + b.sourceRef().TurnID + ":" + callID
	}
	id := b.addUnit(item, contextengine.ToolResult, contextengine.Keep, false, group)
	b.receiptInfo[id] = b.callInfo[callID]
	b.trackContinuationReceipt(group, callID, id, running)
	replay := b.native[id]
	replay.origin = b.callOrigins[callID]
	replay.update = b.receiptSent[callID]
	b.native[id] = replay
	if running {
		b.receiptSent[callID] = true
	}
	return id
}

// deriveItems is a pure projection of recording order, from the active view.
// Representations are memoized until an explicit view/provider/model change.
func (b *builder) deriveItems() []llm.Item {
	items, _ := b.deriveHistory(b.projectionUnitsAfter(""))
	return items
}

func (b *builder) deriveHistory(units []contextengine.Unit) ([]llm.Item, int) {
	items := b.contextPrefix()
	delivered := 0
	if b.window.deliveredThrough != "" {
		delivered = len(items)
	}
	for _, u := range units {
		if !b.includesUnit(u) {
			continue
		}
		rendered := b.deriveUnit(u)
		items = append(items, rendered...)
		if u.ID <= b.window.deliveredThrough {
			delivered = len(items)
		}
		if u.ID == b.claudeBatchThrough && len(rendered) > 0 {
			items[len(items)-1].ClaudeResumeBoundary = true
		}
	}
	return items, delivered
}

func (b *builder) deriveUnit(u contextengine.Unit) []llm.Item {
	if cached, ok := b.derived[u.ID]; ok {
		return cached
	}
	var fit *int
	if u.ID <= b.window.fitThrough {
		fit = windowFitForUnit(u, b.window.fitExcerptChars, b.window.fitPreserveInputs)
	}
	rendered := b.renderUnit(u, b.excerptChars(), fit)
	for i := range rendered {
		rendered[i].Responses = &llm.ItemEncoding{}
	}
	b.derived[u.ID] = rendered
	return rendered
}

func (b *builder) renderUnit(u contextengine.Unit, excerptChars int, fitChars *int) []llm.Item {
	replay := b.native[u.ID]
	if replay.only || replay.legacyNote {
		return nil
	}
	portable := replay.update || b.textOnly || replay.origin != b.provider || replay.model != "" && replay.model != b.request.Model.ID || b.portableBefore != "" && u.ID <= b.portableBefore
	if g := b.continuations[u.Group]; g != nil {
		portable = portable || g.portable
	}
	render := func(fit *int) llm.Item {
		view := u
		switch value := view.Item.Data.(type) {
		case llm.ToolResult:
			view.Item.Data = b.excerptResult(u, value, excerptChars, fit)
		case llm.Message:
			if fit != nil {
				view.Item.Data = b.excerptMessage(u, value, *fit)
			}
		}
		if portable {
			return contextengine.Portable(view)
		}
		return view.Item
	}
	baseline := render(nil)
	if fitChars != nil {
		candidate := render(fitChars)
		if b.windowItemCost(candidate) < b.windowItemCost(baseline) {
			return []llm.Item{candidate}
		}
	}
	return []llm.Item{baseline}
}

func (b *builder) buildDerived() (Result, error) {
	if b.engine == nil {
		return b.Build()
	}
	if _, safe := contextengine.PublicItem(b.committedPrefix[0]); !safe {
		return Result{}, &contextengine.Error{Code: "sensitive_instructions"}
	}
	if _, err := b.requiredContinuations(); err != nil {
		return Result{}, err
	}
	request := b.request
	request.PublicHistory = true
	request.Tools = slices.Clone(request.Tools)
	slices.SortFunc(request.Tools, func(a, b llm.Tool) int { return cmp.Compare(a.Name, b.Name) })
	var delivered int
	units := b.projectionUnitsAfter("")
	request.Input, delivered = b.deriveHistory(units)
	if b.windowPrompt != nil {
		request.Window = &llm.ConversationWindow{Boundary: string(b.window.boundary), DeliveredItems: delivered}
	}
	runtime := b.contextRuntime
	runtime.Provider, runtime.Model = b.provider, b.request.Model.ID
	budget, err := b.contextConfig.ForProvider(runtime.Provider).BudgetFor(runtime, 0)
	if err != nil {
		return Result{}, err
	}
	// Tool schemas are already included in the payload token count. Reserving
	// them again would charge the same input twice.
	used, estimate, delta, measurement := b.inputTokens()
	overBudget := used > budget.Input
	total, excluded := b.engine.UnitCounts()
	d := contextengine.Diagnostics{Version: contextengine.Version, Provider: b.provider, Model: request.Model.ID, Effort: string(request.Model.ReasoningEffort), RuntimeRevision: runtime.Revision, ThroughSequence: b.engine.State().ThroughSequence, CanonicalUnits: total, Budget: budget, EstimatedInputTokens: estimate, RequestBytes: contextengine.JSONBytes(request), InputTokens: used, MeasuredInputTokens: b.lastInput, EstimatedDeltaTokens: delta, Measurement: measurement, Estimator: b.tokenEstimator().Profile(), HeaderFingerprint: b.admissionFingerprint(b.estimate.header), Cache: "append-only"}
	if measurement == "estimated" {
		d.MeasuredInputTokens = 0
	}
	d.Utilization = 100 * float64(used) / float64(budget.Input)
	if overBudget {
		return Result{Report: Report{Context: &d}}, fmt.Errorf("%w: input %d tokens, budget %d tokens",
			&contextengine.Error{Code: "required_context_exceeds_budget"}, used, budget.Input)
	}
	p := contextengine.Package{Version: contextengine.Version, Runtime: runtime, Instructions: request.Input[0], Budget: budget, EstimatedTokens: estimate}
	for _, u := range units {
		if !b.includesUnit(u) {
			continue
		}
		p.Selected = append(p.Selected, contextengine.Selected{Unit: u, Reason: "history"})
		p.IncludedUnitIDs = append(p.IncludedUnitIDs, u.ID)
		d.Recent++
	}
	d.Excluded, d.Omitted = excluded, total-excluded-d.Recent
	return Result{Request: request, Report: Report{Context: &d}, Package: &p}, nil
}

func (b *builder) commitContext() {
	// Legacy histories recorded pending tool status before the original Turn,
	// without a separate projection record. That Turn fixes its receipt view.
	for _, id := range b.pendingOrder {
		if result, ok := b.pendingReceipts[id]; ok {
			b.appendReceipt(llm.Item{Type: llm.ItemToolResult, Data: result}, true)
			delete(b.pendingReceipts, id)
		}
	}
	b.pendingOrder = nil
	if t, ok := b.source.Data.(session.Turn); ok {
		if t.Type == session.TurnRegular && t.ToolContinuation == "" {
			b.windowTurn = windowTurn{id: t.ID, through: b.lastUnit, request: b.lastHumanInput}
			if b.window.header == "" && b.windowPrompt != nil && t.ContextStrategy != contextengine.WindowThreeLayer {
				b.rebuildHistoricalWindow = true
			}
		}
		if t.Type == session.TurnCompaction {
			b.rebuildHistoricalWindow = true
		}
		if b.windowPrompt == nil && t.ContextAdmission != nil && t.ContextAdmission.WindowBoundary == b.window.boundary {
			b.window.firstRequest = false
		}
		b.turnBounds[t.ID] = b.lastUnit
		// A turn without a fingerprint cannot anchor measured prefix usage.
		if t.ContextAdmission != nil && t.ContextAdmission.HeaderFingerprint != "" {
			estimate := b.requestEstimate()
			if t.ContextAdmission.HeaderFingerprint == b.admissionFingerprint(estimate.header) {
				b.turnEstimates[t.ID] = estimate
			}
		}
	}
}

func ContextDebug(r Result) []string {
	if r.Package == nil {
		return nil
	}
	var result []string
	for _, s := range r.Package.Selected {
		result = append(result, s.Unit.ID+":history")
	}
	return result
}

// ApplyContext replays a durable projection decision. Original units stay intact.
func (b *builder) ApplyContext(r sessionstore.ContextRecord) {
	if r.Policy != nil {
		b.recordedContextPolicy = r.Policy
	}
	switch r.Kind {
	case sessionstore.ContextWindowStart, sessionstore.ContextWindowBoundary:
		b.applyWindow(r)
	case sessionstore.ContextSelectionContinuation:
		b.selectionContinuation = true
	case sessionstore.ContextWindowFallback, "summary", "summary_failed":
		b.rebuildHistoricalWindow = true
	case "provider_conversation":
		if c := r.Conversation; c != nil && (c.Compacted || c.Phase == "compact") {
			b.rebuildHistoricalWindow = true
		}
	case "peer_format":
		if r.Text == "quoted-v1" {
			b.legacyPeerFormat = false
		}
	case "pending_receipt":
		for _, item := range r.Items {
			if result, ok := item.Data.(llm.ToolResult); ok {
				b.appendReceipt(item, true)
				delete(b.pendingReceipts, result.CallID)
			}
		}
	case "header", "state":
		if r.Text != "" {
			b.addLegacyNote(r.Text)
		}
	case "projection", "prune":
		for _, p := range r.Projections {
			b.applyHistoricalToolProjection(p.UnitID, p.Items)
		}
		if r.UnitID != "" {
			b.applyHistoricalToolProjection(r.UnitID, r.Items)
		}
	case "runtime":
		b.portableBefore = r.Through
		clear(b.derived)
		b.estimate = derivedEstimate{}
		b.addUnit(llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "Runtime changed to " + r.Provider + " / " + r.Model + "."}}, contextengine.UserMessage, contextengine.Keep, false, "")
	}
}

func (b *builder) addLegacyNote(text string) {
	id := b.addUnit(windowStateItem(text), contextengine.UserMessage, contextengine.Keep, false, "")
	replay := b.native[id]
	replay.legacyNote = true
	b.native[id] = replay
}

// Old replacements can identify a group which must be carried as quoted
// history. The layered window reads the original public receipt.
func (b *builder) applyHistoricalToolProjection(id string, items []llm.Item) {
	for _, item := range items {
		public, safe, redaction := contextengine.PublicItemWithRedaction(item)
		b.queueRedaction(id, redaction)
		if !safe || (item.Type != llm.ItemToolCall && item.Type != llm.ItemMessage) || contextengine.Text(item) == contextengine.Text(public) {
			continue
		}
		if u, ok := b.engine.ResolveUnit(id); ok {
			if group := b.continuations[u.Group]; group != nil && !group.portable {
				group.portable = true
				clear(b.derived)
				b.estimate = derivedEstimate{}
			}
		}
	}
}

// PrepareContext persists the window before it is used by a model request.
func (b *builder) PrepareContext(ctx context.Context, persist func(sessionstore.ContextRecord) error, providerOverflow bool) error {
	if b.engine == nil {
		return nil
	}
	for _, id := range b.pendingOrder {
		if result, ok := b.pendingReceipts[id]; ok {
			if err := persist(sessionstore.ContextRecord{Kind: "pending_receipt", Items: []llm.Item{{Type: llm.ItemToolResult, Data: result}}}); err != nil {
				return err
			}
		}
	}
	b.pendingOrder = nil
	if err := b.recordRuntimeChange(persist); err != nil {
		return err
	}
	_, err := b.prepareWindow(ctx, persist, providerOverflow, 0, nil)
	return err
}

func (b *builder) recordRuntimeChange(persist func(sessionstore.ContextRecord) error) error {
	if b.pendingRuntime == nil {
		return nil
	}
	r := *b.pendingRuntime
	r.Provider, r.Model = b.provider, b.request.Model.ID
	if err := persist(r); err != nil {
		return err
	}
	b.pendingRuntime = nil
	return nil
}

// Translators bound these canonical outputs before wrapping them with status,
// file paths and typed metadata. Reapplying a character limit would destroy that
// framing and can cut a running placeholder down to a few characters.
func (b *builder) receiptAlreadyBounded() bool {
	status, ok := b.source.Data.(sessionstore.ToolCallStatus)
	if !ok {
		return false
	}
	if status.Status.ErrorTruncated {
		return true
	}
	for _, op := range status.Operations {
		if op.Type == operation.TypeShell {
			return true
		}
		if op.Type == operation.TypeRemoteJob {
			if state, err := operation.DecodeRemoteJobState(op); err == nil && state.Plan.Type == files.PlanType {
				return true
			}
		}
	}
	return false
}

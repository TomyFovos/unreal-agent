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
	return e, nil
}

func (b *builder) SetContextRuntime(s sessionstore.RuntimeSelection, tools bool) {
	b.contextRuntime = contextengine.Runtime{Provider: s.Provider, Model: s.Model, Effort: string(s.Effort), Revision: s.Revision, ContextWindow: s.ContextWindow, Tools: tools}
	if b.engine != nil {
		b.engine.Configure(b.contextRuntime, b.constraints, b.instructions != "")
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
	var private []llm.Item
	for _, item := range response.Output {
		if item.Type == llm.ItemReasoning {
			private = append(private, item)
		}
	}
	attach := -1
	for i, item := range response.Output {
		if item.Type == llm.ItemToolCall {
			attach = i
			break
		}
	}
	if attach < 0 {
		for i, item := range response.Output {
			if item.Type == llm.ItemMessage {
				attach = i
				break
			}
		}
	}
	for i, item := range response.Output {
		b.sourceOutputIndex = i
		kind, group := contextengine.ModelMessage, ""
		switch v := item.Data.(type) {
		case llm.Message:
		case llm.ToolCall:
			kind = contextengine.ToolRequest
			group = b.historyProvider + ":" + b.sourceRef().TurnID + ":" + v.CallID
			b.callGroups[v.CallID] = group
			_, eligible := contextengine.PublicItem(item)
			b.blockedCalls[v.CallID] = !eligible
		default:
			continue
		}
		var extra []llm.Item
		if i == attach {
			extra = private
		}
		b.addUnit(item, kind, contextengine.Keep, false, false, group, extra)
	}
	if response.Failure != nil {
		b.sourceOutputIndex = len(response.Output)
		b.addUnit(llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: response.Failure.Message}}, contextengine.ErrorUnit, contextengine.Pin, false, false, "", nil)
	}
}

func (b *builder) contextReceipt(callID string, output []llm.ToolResultOutput, running bool) {
	if b.engine == nil {
		return
	}
	for _, id := range b.runningUnits[callID] {
		b.engine.RemoveStaged(id)
	}
	delete(b.runningUnits, callID)
	item := llm.Item{Type: llm.ItemToolResult, Data: llm.ToolResult{CallID: callID, Output: output}}
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
	if b.blockedCalls[callID] {
		class = contextengine.Omit
	}
	id := b.addUnit(item, contextengine.ToolResult, class, true, image && !b.textOnly, group, nil)
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
	u := s.Unit
	native := b.native[u.ID]
	if s.ReferenceOnly {
		text := contextengine.ReferenceText(u)
		if !b.textOnly && native.origin == b.provider && s.Reason != "retrieved" {
			if receipt, ok := u.Item.Data.(llm.ToolResult); ok {
				return []llm.Item{{Type: llm.ItemToolResult, Data: llm.ToolResult{CallID: receipt.CallID, Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: text}}}}}
			}
		}
		return []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: text}}}
	}
	if b.textOnly || native.origin != b.provider || s.Reason == "retrieved" || s.Reason == "recent-portable" || u.RetrievalOnly {
		return []llm.Item{contextengine.Portable(u, s.Reason == "retrieved")}
	}
	item := u.Item
	item.ProviderID = native.providerID
	return append(slices.Clone(native.extra), item)
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
	if b.engine == nil {
		return b.Build()
	}
	return b.buildCompactedReserve(reserve)
}

func (b *builder) buildCompactedReserve(reserve int64) (Result, error) {
	runtime := b.contextRuntime
	runtime.Model = b.request.Model.ID
	runtime.Effort = string(b.request.Model.ReasoningEffort)
	runtime.Provider = b.provider
	runtime.Tools = len(b.request.Tools) > 0
	schema := int64(0)
	if len(b.request.Tools) > 0 {
		schema = contextengine.Estimate(b.request.Tools) + 512
	}
	cost := func(u contextengine.Unit, reason string, ref bool) int64 {
		// Use a worst-case charge so changing an incomplete group to quoted
		// historical evidence cannot exceed the selector's budget.
		if ref {
			return contextengine.Estimate(llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: contextengine.ReferenceText(u)}}) + 128
		}
		charge := u.Tokens
		private := b.native[u.ID]
		if !b.textOnly && private.origin == b.provider {
			charge += private.charge
		}
		return charge
	}
	p, d, err := b.engine.Build(contextengine.BuildInput{Runtime: runtime, Instructions: b.committedPrefix[0], SchemaTokens: schema, Cost: cost, TransportReserve: reserve})
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
		request.Input = append(request.Input, b.renderSelected(selected)...)
		if selected.ReferenceOnly {
			changes = append(changes, Change{Kind: ChangeOmitted, Source: selected.Unit.ID, Reason: "referenced"})
		}
	}
	// A final renderer check covers the actual JSON/framing estimate as well.
	actualEstimate := int64(0)
	for _, item := range request.Input {
		actualEstimate += contextengine.Estimate(item)
	}
	if actualEstimate > p.Budget.Input {
		return Result{}, &contextengine.Error{Code: "required_context_exceeds_budget"}
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

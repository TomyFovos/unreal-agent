package contextbuilder

import (
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/contextstate"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/record"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

type windowSource struct {
	session session.ID
	state   func(context.Context) (contextstate.State, sessionstore.Sequence, error)
}

type windowProjection struct {
	header, through      string
	deliveredThrough     string
	retained             []string
	state                llm.Item
	excerptChars         int
	fitExcerptChars      *int
	fitThrough           string
	boundary             record.Locator
	firstRequest         bool
	midTurn              bool
	afterTokens          int64
	minimumExceedsTarget bool
}

type windowRequest struct {
	id   inbox.ID
	unit string
}

type windowTurn struct {
	id      session.TurnID
	through string
	request windowRequest
}

type windowTail struct {
	through        string
	retained       []string
	units          []contextengine.Unit
	items          []llm.Item
	omittedThrough sessionstore.Sequence
}

// SetWindowSource supplies a detached state snapshot at a committed read boundary.
func (b *builder) SetWindowSource(id session.ID, state func(context.Context) (contextstate.State, sessionstore.Sequence, error)) {
	b.windowSource = windowSource{session: id, state: state}
}

func (b *builder) SetWindowPrompt(render llm.WindowPrompt) {
	// Runtime selection reapplies the same renderer at each request. A new
	// provider resets the estimate in SetRuntimeProvider.
	if (b.windowPrompt == nil) != (render == nil) {
		b.estimate = derivedEstimate{}
		clear(b.derived)
	}
	b.windowPrompt = render
}

func (b *builder) UsesThreeLayerContext() bool { return b.engine != nil }

func (b *builder) ContextStrategy() string {
	if b.engine != nil {
		return contextengine.WindowThreeLayer
	}
	return ""
}

func (b *builder) FirstWindowRequest() record.Locator {
	if b.window.firstRequest {
		return b.window.boundary
	}
	return ""
}

func (b *builder) excerptChars() int {
	if b.window.header != "" {
		return b.window.excerptChars
	}
	return b.contextConfig.ToolOutputExcerptChars
}

func (b *builder) contextPrefix() []llm.Item {
	system, _ := contextengine.PublicItem(b.committedPrefix[0])
	items := []llm.Item{system}
	if b.window.state.Type != "" {
		items = append(items, b.window.state)
	}
	return items
}

func (b *builder) includesUnit(u contextengine.Unit) bool { return b.window.includes(u) }

func (w windowProjection) includes(u contextengine.Unit) bool {
	if u.Class == contextengine.Omit {
		return false
	}
	if u.ID > w.through {
		return true
	}
	_, found := slices.BinarySearch(w.retained, u.ID)
	return found
}

func (b *builder) applyWindow(r sessionstore.ContextRecord) {
	if r.Window == nil {
		return
	}
	b.rebuildHistoricalWindow = false
	b.selectionContinuation = false
	delivered := b.window.deliveredThrough
	retained := slices.Clone(r.Retained)
	slices.Sort(retained)
	b.window = windowProjection{
		header: r.Header, through: r.Through, excerptChars: r.Window.ToolOutputExcerptChars,
		retained: slices.Compact(retained),
	}
	if r.Kind == sessionstore.ContextWindowBoundary {
		b.window.state = windowStateItem(r.Text)
		b.window.state.Responses = &llm.ItemEncoding{}
		b.window.midTurn = slices.Contains(r.Window.Causes, sessionstore.WindowCauseMidTurn)
		b.window.afterTokens = r.Window.AfterTokens
		b.window.fitExcerptChars = r.Window.FitExcerptChars
		b.window.fitThrough = r.Window.FitThrough
		b.window.minimumExceedsTarget = r.Window.MinimumExceedsTarget
		if r.Window.HeaderTokens == nil && b.windowPrompt != nil && r.Provider == b.provider {
			// The legacy private-conversation metric counted just the text body.
			system, _ := contextengine.PublicItem(b.committedPrefix[0])
			b.window.afterTokens += b.windowHeaderTokens(system)
		}
		if b.windowSource.session != "" && b.source.Sequence > 0 {
			b.window.boundary = record.Item(string(b.windowSource.session), uint64(b.source.Sequence))
			b.window.firstRequest = true
		}
	} else {
		b.window.deliveredThrough = delivered
	}
	clear(b.derived)
	b.estimate = derivedEstimate{}
}

// projectionUnitsAfter skips history already displaced from L1. Retained
// units precede the raw suffix in canonical projection order.
func (b *builder) projectionUnitsAfter(after string) []contextengine.Unit {
	through, retained := b.window.through, b.window.retained
	if after >= through {
		return b.engine.UnitsAfter(after)
	}
	suffix := b.engine.UnitsAfter(through)
	var units []contextengine.Unit
	for _, id := range retained {
		if id > after && id <= through {
			if u, ok := b.engine.ResolveUnit(id); ok {
				units = append(units, u)
			}
		}
	}
	if len(units) == 0 {
		return suffix
	}
	return append(units, suffix...)
}

func windowStateItem(text string) llm.Item {
	return llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: text}}
}

func (b *builder) windowHeader() (string, error) {
	data, err := json.Marshal(struct {
		System   llm.Item
		Tools    []llm.Tool
		Model    llm.Model
		Provider string
		Revision uint64
	}{b.committedPrefix[0], b.request.Tools, b.request.Model, b.provider, b.contextRuntime.Revision}, json.Deterministic(true))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}

// PrepareToolWindow runs on the Coordinator after it commits the batch receipts.
// The CLI is waiting for the hook reply and has not read the batch yet.
func (b *builder) PrepareToolWindow(ctx context.Context, persist func(sessionstore.ContextRecord) error, inputTokens int64, projectQueued func() error) (bool, error) {
	if b.engine == nil || b.windowPrompt == nil || inputTokens <= 0 {
		return false, nil
	}
	return b.prepareWindow(ctx, persist, false, inputTokens, projectQueued)
}

func (b *builder) prepareWindow(ctx context.Context, persist func(sessionstore.ContextRecord) error, providerOverflow bool, toolInputTokens int64, projectQueued func() error) (bool, error) {
	budget, err := b.contextConfig.BudgetFor(b.contextRuntime, 0)
	if err != nil {
		return false, err
	}
	midTurn := toolInputTokens > 0
	before, estimate, measurement := toolInputTokens, int64(0), "provider-reported"
	if !midTurn {
		before, estimate, _, measurement = b.inputTokens()
	}
	threshold := b.contextConfig.WindowThresholdTokens
	growth := before
	if b.window.state.Type != "" {
		if midTurn {
			estimate = b.estimateRequest()
		}
		// Compare estimates in the same window. Provider measurements include
		// provider-owned context and still decide strict input admission.
		growth = estimate - b.window.afterTokens
	}
	atThreshold, overBudget := growth >= threshold, before > budget.Input
	header, err := b.windowHeader()
	if err != nil {
		return false, err
	}
	headerChanged := b.window.header != "" && b.window.header != header
	headerRebuild := headerChanged || b.selectionContinuation || b.rebuildHistoricalWindow
	if !midTurn && b.window.midTurn && b.window.firstRequest && !overBudget && !providerOverflow && !headerRebuild {
		return false, nil // use the just-committed tool boundary for its first request
	}
	if !atThreshold && !overBudget && !providerOverflow && !headerRebuild {
		if midTurn || b.window.header != "" {
			return false, nil
		}
		return false, persist(sessionstore.ContextRecord{
			Kind: sessionstore.ContextWindowStart, Header: header,
			Provider: b.provider, Model: b.request.Model.ID,
			Window: &sessionstore.WindowBoundary{ToolOutputExcerptChars: b.contextConfig.ToolOutputExcerptChars,
				ThresholdTokens: threshold, L2TargetTokens: b.contextConfig.L2TargetTokens, RecentEventLimit: &b.contextConfig.RecentEventCount},
		})
	}

	if projectQueued != nil {
		if err := projectQueued(); err != nil {
			return false, err
		}
	}
	units := b.engine.Units()
	groups, err := b.requiredContinuations()
	if err != nil {
		return false, err
	}
	state, recent, seq, err := b.renderWindowState(ctx)
	if err != nil {
		return false, err
	}
	system, _ := contextengine.PublicItem(b.committedPrefix[0])
	estimator := b.tokenEstimator()
	headTokens := b.windowHeaderTokens(system)
	limit := min(threshold-1, budget.Input)
	preferred, minimum := b.windowCuts(units)
	if providerOverflow {
		preferred = minimum
	}
	excerpt := b.contextConfig.ToolOutputExcerptChars
	tail := b.selectWindowTail(units, groups, preferred)
	b.renderWindowTail(&tail, excerpt, nil)
	l2, after, head := b.sizeWindow(headTokens, state, tail)
	if after > limit && minimum != preferred {
		// The old last-user tail is too large. Move to the newest cut and
		// retain only the in-flight turn, queued inputs and whole dependencies.
		tail = b.selectWindowTail(units, groups, minimum)
		b.renderWindowTail(&tail, excerpt, nil)
		l2, after, head = b.sizeWindow(headTokens, state, tail)
	}
	requiredInput, requiredTail := after, after-head
	minimumExceedsTarget := after > limit
	var fitChars *int
	if after > budget.Input {
		fitChars, after = b.fitWindowTail(&tail, excerpt, l2, headTokens, budget.Input)
		if fitChars == nil {
			return false, fmt.Errorf("%w: input budget %d tokens, system and tools %d, L2 %d, required L1 tail %d, smallest L1 tail %d (minimum total %d)",
				&contextengine.Error{Code: "required_context_exceeds_budget"}, budget.Input, headTokens, head-headTokens, requiredTail, after-head, after)
		}
	}
	if after <= budget.Input && !overBudget && !midTurn && !headerRebuild && !providerOverflow && b.window.minimumExceedsTarget && minimumExceedsTarget &&
		after >= estimate && tail.through == b.window.through && slices.Equal(tail.retained, b.retainedWindowUnits(units)) {
		return false, nil // another boundary cannot reduce the same required input
	}
	tailStats := &sessionstore.WindowTail{
		Tokens: max(0, after-head), Items: int64(len(tail.items)),
	}
	if after <= budget.Input && b.window.header != "" && b.windowSource.session != "" {
		previous := b.windowEvidence(units)
		tailStats.Previous = &previous
	}
	var causes []string
	if atThreshold {
		causes = append(causes, "threshold")
	}
	if overBudget {
		causes = append(causes, "admission")
	}
	if headerChanged {
		causes = append(causes, "header_change")
	}
	if providerOverflow {
		causes = append(causes, "provider_overflow")
	}
	if midTurn || b.selectionContinuation {
		causes = append(causes, sessionstore.WindowCauseMidTurn)
	}
	if b.selectionContinuation {
		causes = append(causes, sessionstore.WindowCauseSelectionChange)
	}
	if b.rebuildHistoricalWindow {
		causes = append(causes, "historical_context")
	}
	record := sessionstore.ContextRecord{
		Kind: sessionstore.ContextWindowBoundary, Header: header, Text: contextengine.Text(l2),
		From: b.window.through, Through: tail.through, Retained: tail.retained,
		Provider: b.provider, Model: b.request.Model.ID,
		Window: &sessionstore.WindowBoundary{
			Causes: causes, BeforeTokens: before, AfterTokens: after, BeforeMeasurement: measurement,
			Estimator: estimator.Profile(), ThresholdTokens: threshold,
			HeaderTokens: &headTokens, MinimumExceedsTarget: minimumExceedsTarget,
			L2Tokens: head - headTokens, L2TargetTokens: b.contextConfig.L2TargetTokens,
			RecentEvents: recent, CacheRewarmMinutes: b.contextConfig.CacheRewarmMinutes,
			RecentEventLimit:       &b.contextConfig.RecentEventCount,
			ToolOutputExcerptChars: excerpt, SnapshotSequence: seq,
			Tail: tailStats,
		},
	}
	if fitChars != nil {
		record.Window.Fit = sessionstore.WindowFitExcerpted
		record.Window.FitExcerptChars = fitChars
		record.Window.FitThrough = b.lastUnit
		record.Window.RequiredInputTokens = requiredInput
		record.Window.RequiredTailTokens = requiredTail
		record.Window.InputBudgetTokens = budget.Input
	}
	err = persist(record)
	return err == nil, err
}

func (b *builder) windowHeaderTokens(system llm.Item) int64 {
	// System instructions and tool schemas are separate from the history
	// rendered by a private-conversation adapter.
	e := contextengine.TokenEstimator{}
	return e.Item(system) + e.Tools(b.request.Tools)
}

func (b *builder) sizeWindow(headerTokens int64, state string, tail windowTail) (llm.Item, int64, int64) {
	heading := "Unreal current state at the window boundary (recorded data):"
	if tail.omittedThrough > 0 && b.windowSource.session != "" {
		id := string(b.windowSource.session)
		heading += fmt.Sprintf(" Earlier conversation is in L3. Use record show or record find for %s through %s.",
			record.Item(id, 1), record.Item(id, uint64(tail.omittedThrough)))
	}
	l2 := windowStateItem(heading + "\n" + state)
	if b.windowPrompt != nil {
		size := b.measureWindowPrompt([]llm.Item{l2})
		l2Tokens := size.tokens()
		size.add(b.measureWindowPrompt(tail.items))
		return l2, headerTokens + size.tokens(), headerTokens + l2Tokens
	}
	head := headerTokens + b.tokenEstimator().Item(l2)
	return l2, head + b.tokenEstimator().Items(tail.items), head
}

func (b *builder) windowEvidence(units []contextengine.Unit) []record.Locator {
	var refs []record.Locator
	seen := make(map[record.Locator]bool)
	add := func(a record.Address) {
		a.Session = string(b.windowSource.session)
		loc := a.Locator()
		if !seen[loc] {
			seen[loc] = true
			refs = append(refs, loc)
		}
	}
	for _, u := range units {
		if !b.window.includes(u) || u.Source.Sequence == 0 || u.Source.Sequence < b.forkSequence {
			continue
		}
		a := record.Address{Sequence: uint64(u.Source.Sequence), Kind: record.SessionItem}
		switch u.Source.SourceType {
		case string(sessionstore.ItemInput):
			if u.ID > b.deliveredThrough {
				continue // still queued for delivery, even when already in the builder
			}
			switch u.Kind {
			case contextengine.UserMessage:
				a.Kind = record.UserMessage
			case contextengine.ChildResult:
				a.Kind = record.PeerMessage
			default:
				continue
			}
		case string(sessionstore.ItemModelResponse):
			a.Part = u.Source.OutputIndex
			switch u.Item.Data.(type) {
			case llm.Message:
				a.Kind = record.AssistantMessage
			case llm.ToolCall:
				a.Kind = record.ToolCall
			default:
				continue
			}
			if b.native[u.ID].only || b.native[u.ID].legacyNote {
				continue
			}
		case string(sessionstore.ItemToolCallStatus):
			info := b.receiptInfo[u.ID]
			for _, stream := range info.streams {
				add(stream)
			}
			if info.directError {
				a.Kind = record.ToolError
			}
		default:
			continue
		}
		add(a)
		add(record.Address{Sequence: a.Sequence, Kind: record.SessionItem})
	}
	return refs
}

func (b *builder) retainedWindowUnits(units []contextengine.Unit) []string {
	if len(b.window.retained) == 0 {
		return nil
	}
	var ids []string
	for _, u := range units {
		if u.ID > b.window.through {
			break
		}
		if b.window.includes(u) {
			ids = append(ids, u.ID)
		}
	}
	return ids
}

func (b *builder) windowCuts(units []contextengine.Unit) (preferred, minimum string) {
	minimum = b.lastUnit
	if b.windowTurn.id != "" {
		minimum = b.windowTurn.through
	}
	if b.lastHumanInput.unit != "" {
		for _, u := range units {
			if u.ID >= b.lastHumanInput.unit || b.undeliveredWindowInput(u) {
				break
			}
			preferred = u.ID
		}
	}
	return min(max(preferred, b.window.through), minimum), minimum
}

func (b *builder) undeliveredWindowInput(u contextengine.Unit) bool {
	return u.ID > b.deliveredThrough && u.Class != contextengine.Omit && u.Source.SourceType == string(sessionstore.ItemInput) &&
		(u.Kind == contextengine.UserMessage || u.Kind == contextengine.ChildResult)
}

func (b *builder) selectWindowTail(units []contextengine.Unit, required map[string]bool, through string) windowTail {
	groups := maps.Clone(required)
	for _, u := range units {
		if u.ID > through && u.Group != "" && u.Class != contextengine.Omit {
			groups[u.Group] = true
		}
	}
	tail := windowTail{through: through}
	for _, u := range units {
		if u.Class == contextengine.Omit {
			continue
		}
		if u.ID <= through {
			if !groups[u.Group] && !b.undeliveredWindowInput(u) {
				tail.omittedThrough = max(tail.omittedThrough, u.Source.Sequence)
				continue
			}
			tail.retained = append(tail.retained, u.ID)
		}
		tail.units = append(tail.units, u)
	}
	return tail
}

func (b *builder) renderWindowTail(tail *windowTail, excerpt int, fit *int) {
	tail.items = tail.items[:0]
	for _, u := range tail.units {
		tail.items = append(tail.items, b.renderUnit(u, excerpt, fit)...)
	}
}

// The minimum cut has already removed optional history. Find the largest
// common character allowance that fits without changing the retained units.
func (b *builder) fitWindowTail(tail *windowTail, excerpt int, l2 llm.Item, header, budget int64) (*int, int64) {
	size := func(chars int) int64 {
		b.renderWindowTail(tail, excerpt, &chars)
		if b.windowPrompt != nil {
			text := b.measureWindowPrompt([]llm.Item{l2})
			text.add(b.measureWindowPrompt(tail.items))
			return header + text.tokens()
		}
		e := b.tokenEstimator()
		return header + e.Item(l2) + e.Items(tail.items)
	}
	minimum := size(0)
	if minimum > budget {
		return nil, minimum
	}
	// Bytes are an upper bound on rune count. At this allowance there are no
	// adaptive excerpts, so the already measured baseline exceeds the budget.
	var upper int
	for _, u := range tail.units {
		switch value := u.Item.Data.(type) {
		case llm.Message:
			upper = max(upper, len(value.Text))
		case llm.ToolResult:
			for _, part := range value.Output {
				if part.Kind == llm.ToolResultText {
					upper = max(upper, len(part.Value))
				}
			}
		}
	}
	lo, hi := 0, upper
	for hi-lo > 1 {
		mid := lo + (hi-lo)/2
		if size(mid) <= budget {
			lo = mid
		} else {
			hi = mid
		}
	}
	return &lo, size(lo)
}

// Compare candidate items before joining Claude's prompt lines so rounding
// cannot cause an excerpt to increase the size of the complete prompt.
func (b *builder) windowItemCost(item llm.Item) int64 {
	if b.windowPrompt != nil {
		line, ok := b.windowPrompt(item)
		if !ok {
			return 0
		}
		return textWeight(line)
	}
	return b.tokenEstimator().Item(item)
}

func (b *builder) renderWindowState(ctx context.Context) (string, int, sessionstore.Sequence, error) {
	var state contextstate.State
	var seq sessionstore.Sequence
	if b.windowSource.state != nil {
		var err error
		state, seq, err = b.windowSource.state(ctx)
		if err != nil {
			return "", 0, 0, err
		}
	}
	state.Session.ID = string(b.windowSource.session)
	state.Session.Provider, state.Session.Model = b.provider, b.request.Model.ID
	state.Session.Effort = string(b.request.Model.ReasoningEffort)
	if b.windowTurn.request != b.lastHumanInput {
		b.fillWindowRequest(&state, b.windowTurn.request)
	}
	b.fillWindowRequest(&state, b.lastHumanInput)
	for id, request := range state.Requests.Current {
		if request.Status == "" && id != string(b.lastHumanInput.id) && id != string(b.windowTurn.request.id) {
			delete(state.Requests.Current, id)
		}
	}
	state = state.Render(b.contextConfig.RecentEventCount)
	raw, err := state.MarshalJSON()
	if err != nil {
		return "", 0, 0, err
	}
	item, _ := contextengine.PublicItem(windowStateItem(string(raw)))
	return contextengine.Text(item), len(state.RecentEvents), seq, nil
}

func (b *builder) fillWindowRequest(state *contextstate.State, src windowRequest) {
	unit, ok := b.engine.ResolveUnit(src.unit)
	if !ok {
		return
	}
	id := string(src.id)
	request, observed := state.Requests.Current[id]
	if observed && (request.Origin != contextstate.HumanInput ||
		request.Status != "" && request.Status != contextstate.RequestActive) {
		return
	}
	if !observed {
		request = contextstate.Request{ID: id, Origin: contextstate.HumanInput, TS: unit.Source.RecordedAt}
		if unit.Source.Sequence > 0 && b.windowSource.session != "" {
			request.Locator = record.Input(string(b.windowSource.session), uint64(unit.Source.Sequence), record.UserMessage)
			if _, done := state.Requests.Processed.Sources[request.Locator]; done {
				return
			}
			request.Sources = []record.Locator{request.Locator}
		}
	}
	request.Status = contextstate.RequestActive
	request.Text = contextengine.Text(unit.Item)
	request.FirstLine, _, _ = strings.Cut(request.Text, "\n")
	request.FirstLine = strings.TrimSuffix(request.FirstLine, "\r")
	request.Chars = utf8.RuneCountInString(request.Text)
	if state.Requests.Current == nil {
		state.Requests.Current = make(map[string]contextstate.Request)
	}
	state.Requests.Current[id] = request
}

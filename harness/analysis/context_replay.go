package analysis

import (
	"encoding/json/v2"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/contextstate"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
	"github.com/unreallabsai/unreal-agent/internal/displaytext"
)

type contextPosition struct {
	after   sessionstore.Sequence
	window  *contextWindowLog
	turn    *contextTurnLog
	request *contextRequest
}

func (p contextPosition) dimensions() ContextDimensions {
	key := p.window.key
	if p.request != nil {
		key = p.request.key
	} else if p.turn != nil {
		key = p.turn.key
	}
	key.Strategy = p.window.key.Strategy
	return key
}

type contextWindowLog struct {
	key        ContextDimensions
	at         time.Time
	sequence   sessionstore.Sequence
	open       string
	boundary   *sessionstore.WindowBoundary
	pending    *contextstate.PendingCounts
	compaction *contextCompactionLog
	l3Covered  bool
	hasTurn    bool
}

type contextTurnLog struct {
	key        ContextDimensions
	window     *contextWindowLog
	after      sessionstore.Sequence
	at, end    time.Time
	latest     *contextRequest // latest reported main request inside a CLI execution
	single     *contextRequest
	firstCache sessionstore.WindowCache
	hasReply   bool
}

type contextRequest struct {
	contextPosition
	key        ContextDimensions
	at         time.Time
	usage      llm.RequestUsage
	countKnown bool
}

type contextCompactionLog struct {
	contextPosition
	key     ContextDimensions
	at      time.Time
	engine  string
	outcome string
	numbers contextCounts
}

type contextStateNote struct {
	after sessionstore.Sequence
	kind  string
}

type contextQuestionCount struct {
	contextPosition
	count int
}

type contextReplay struct {
	id            session.ID
	cursor        sessionstore.Sequence
	windows       []*contextWindowLog
	boundaries    map[sessionstore.Sequence]*contextWindowLog
	positions     []contextPosition
	current       *contextTurnLog
	currentReq    *contextRequest
	turns         map[session.TurnID]*contextTurnLog
	invocations   []*contextTurnLog
	internalTurns map[session.TurnID]bool
	streams       map[string]*contextRequest
	requests      []*contextRequest
	notes         []contextStateNote
	reads         []sessionstore.RecordRead
	compactions   []*contextCompactionLog
	fallbacks     []ContextFallback
	questions     []contextQuestionCount
	childStarts   map[string]sessionstore.Sequence
	subscription  []ContextSubscription
	policies      []contextPolicyEvent
}

func newContextReplay(id session.ID) contextReplay {
	return contextReplay{
		id: id, boundaries: make(map[sessionstore.Sequence]*contextWindowLog),
		turns: make(map[session.TurnID]*contextTurnLog), internalTurns: make(map[session.TurnID]bool),
		streams: make(map[string]*contextRequest), childStarts: make(map[string]sessionstore.Sequence),
	}
}

func (m *contextReplay) window() *contextWindowLog { return m.windows[len(m.windows)-1] }

func (m *contextReplay) startWindow(at time.Time, seq sessionstore.Sequence, key ContextDimensions, kind string) *contextWindowLog {
	w := &contextWindowLog{key: key, at: at, sequence: seq, open: kind}
	m.windows = append(m.windows, w)
	m.boundaries[seq] = w
	m.currentReq = nil
	m.point(seq, nil)
	return w
}

func (m *contextReplay) point(seq sessionstore.Sequence, turn *contextTurnLog) {
	m.positions = append(m.positions, contextPosition{after: seq, window: m.window(), turn: turn, request: m.currentReq})
}

func (m *contextReplay) position(seq sessionstore.Sequence) contextPosition {
	i := sort.Search(len(m.positions), func(i int) bool { return m.positions[i].after > seq })
	return m.positions[max(0, i-1)]
}

func (m *contextReplay) selectStrategy(seq sessionstore.Sequence, at time.Time, key ContextDimensions) {
	w := m.window()
	if key.Strategy == w.key.Strategy {
		return
	}
	if w.open == "initial" && !w.hasTurn && w.key.Strategy == "" {
		w.key = key
		return
	}
	m.startWindow(at, seq, key, "initial")
}

func (m *contextReplay) apply(item host.HistoryItem, active *sessionstore.RuntimeSelection, runtime *sessionMetrics) {
	seq, at := item.Sequence, item.RecordedAt
	key := ContextDimensions{ModelKey: modelKey(active)}
	if _, fork := item.Data.(sessionstore.Fork); fork {
		*m = newContextReplay(m.id)
	}
	m.cursor = seq
	if len(m.windows) == 0 {
		m.startWindow(at, seq, key, "initial")
	}
	key.Strategy = m.window().key.Strategy
	switch v := item.Data.(type) {
	case session.Turn:
		if v.Type == session.TurnCompaction {
			m.internalTurns[v.ID] = true
			return
		}
		if t := m.turns[v.ToolContinuation]; v.ToolContinuation != "" && t != nil {
			m.turns[v.ID] = t
			m.current = t
			m.point(seq, t)
			return
		}
		if v.ContextStrategy != "" {
			if strategy := recordedStrategy(v.ContextStrategy); strategy != "" {
				key.Strategy = strategy
			}
		}
		if key.Strategy == "" {
			key.Strategy = historicalSummaryStrategy
		}
		m.selectStrategy(seq, at, key)
		w := m.window()
		if !w.hasTurn && w.open == "initial" && v.ContextStrategy != "" {
			w.l3Covered = true
		}
		w.hasTurn = true
		t := &contextTurnLog{key: key, window: w, after: seq, at: at}
		m.turns[v.ID], m.current, m.currentReq = t, t, nil
		m.invocations = append(m.invocations, t)
		m.point(seq, t)
	case sessionstore.ModelResponse:
		if !m.internalTurns[v.TurnID] {
			m.response(seq, at, v, runtime.requests[len(runtime.requests)-1])
		}
	case sessionstore.HostRecord:
		switch v.Kind {
		case "configuration":
			var config struct {
				Context struct{ WindowStrategy string }
			}
			if json.Unmarshal(v.Configuration, &config) == nil && recordedStrategy(config.Context.WindowStrategy) != "" {
				key.Strategy = recordedStrategy(config.Context.WindowStrategy)
				m.selectStrategy(seq, at, key)
			}
		case sessionstore.HostRuntimeApplied:
			m.currentReq = nil
			m.point(seq, nil)
		case sessionstore.HostContext:
			if v.Context != nil {
				m.contextRecord(seq, at, key, *v.Context)
			}
		case sessionstore.HostActivity:
			if v.Activity != nil {
				m.activity(seq, at, key, *v.Activity)
			}
		case sessionstore.HostChildReview:
			if v.ChildReview != nil && (v.ChildReview.Decision == "accept" || v.ChildReview.Decision == "reject") {
				m.notes = append(m.notes, contextStateNote{seq, v.ChildReview.Decision})
			}
		}
	case contextstate.Event:
		if kind := stateNoteKind(v); kind != "" {
			m.notes = append(m.notes, contextStateNote{seq, kind})
		}
	case sessionstore.ToolCallStatus:
		for _, op := range v.Operations {
			if op.ToolName == "SubagentStart" {
				m.childStart(string(op.ID), seq)
			}
		}
	case inbox.Input:
		if op, err := sessionstore.DecodeOperationIntent(v); err == nil && op.ToolName == "SubagentStart" {
			m.childStart(string(op.ID), seq)
		}
	}
}

func (m *contextReplay) childStart(handle string, seq sessionstore.Sequence) {
	if _, exists := m.childStarts[handle]; !exists {
		m.childStarts[handle] = seq
	}
}

func stateNoteKind(event contextstate.Event) string {
	if event.Version != contextstate.Version || !strings.HasPrefix(event.ID, "note:") {
		return ""
	}
	switch event.Kind {
	case contextstate.DecisionRecorded:
		return "decide"
	case contextstate.PlanChanged:
		return "plan"
	case contextstate.RuleRecorded:
		return "rule"
	case contextstate.QuestionChanged:
		var update struct {
			Status contextstate.QuestionStatus `json:"status"`
		}
		if json.Unmarshal(event.Data, &update) == nil && update.Status == contextstate.QuestionOpen {
			return "question"
		}
	}
	return ""
}

func (m *contextReplay) contextRecord(seq sessionstore.Sequence, at time.Time, key ContextDimensions, c sessionstore.ContextRecord) {
	if c.Provider != "" {
		key.Provider = viewer.SafeText(c.Provider)
	}
	if c.Model != "" {
		key.Model = viewer.SafeText(c.Model)
	}
	if c.Policy != nil {
		m.policies = append(m.policies, contextPolicyEvent{seq, at, key.Provider, c.Policy})
	}
	switch c.Kind {
	case sessionstore.ContextWindowStart:
		key.Strategy = contextengine.WindowThreeLayer
		w := m.window()
		if w.open == "initial" && !w.hasTurn {
			w.key, w.at, w.open = key, at, "window start"
		} else {
			w = m.startWindow(at, seq, key, "window start")
		}
		w.boundary = c.Window
		w.l3Covered = c.Window != nil && c.Window.RecentEventLimit != nil
	case sessionstore.ContextWindowBoundary:
		key.Strategy = contextengine.WindowThreeLayer
		w := m.startWindow(at, seq, key, "rebuild")
		w.boundary = c.Window
		w.l3Covered = c.Window != nil && c.Window.Tail != nil
		_, raw, ok := strings.Cut(c.Text, "\n")
		var state contextstate.State
		if c.Window != nil && c.Window.Pending != nil {
			w.pending = c.Window.Pending
		} else if ok && json.Unmarshal([]byte(raw), &state) == nil && state.Version == contextstate.Version {
			w.pending = new(state.PendingCounts())
		}
	case sessionstore.ContextWindowFallback:
		if window := c.Window; window != nil && window.Fallback != "" {
			key.Strategy = historicalSummaryStrategy
			m.selectStrategy(seq, at, key)
			n := make(contextCounts)
			measureInput(n, "before_tokens", window.BeforeTokens, window.BeforeMeasurement)
			n.tokens("required_tokens", &window.AfterTokens, "derived")
			n.tokens("header_tokens", window.HeaderTokens, "derived")
			n.tokens("l2_tokens", &window.L2Tokens, "derived")
			n.tokens("l2_target_tokens", &window.L2TargetTokens, "measured")
			n.tokens("input_budget_tokens", &window.InputBudgetTokens, "measured")
			if window.Tail != nil {
				n.tokens("tail_tokens", &window.Tail.Tokens, "derived")
			}
			m.fallbacks = append(m.fallbacks, ContextFallback{ContextDimensions: key, At: at, Reason: window.Fallback, Numbers: n.report()})
		}
	case "header":
		key.Strategy = historicalSummaryStrategy
		m.selectStrategy(seq, at, key)
	case "summary", "summary_failed":
		key.Strategy = historicalSummaryStrategy
		if c.Usage != nil {
			key.Fast = fastLabel(c.Usage.Fast)
		}
		compact := &contextCompactionLog{key: key, at: at, engine: "Unreal", outcome: "completed", numbers: make(contextCounts)}
		if g := c.Generation; g != nil && g.Valid() {
			source := "derived"
			if g.Source == "provider_api" {
				source = "measured"
			}
			compact.numbers.add("generation_seconds", float64(g.DurationMicros)/1e6, source)
		}
		if c.Usage != nil {
			u := responseRequestUsage(*c.Usage)
			compact.numbers.tokens("before_tokens", u.Input, "measured")
			compact.numbers.tokens("summary_output_tokens", u.Output, "measured")
		}
		m.addCompaction(seq, compact, c.Kind == "summary", c.Generation != nil)
	case "provider_conversation":
		if c.Conversation != nil {
			m.cliCompaction(seq, at, key, *c.Conversation)
		}
	case "record_read":
		if c.RecordRead != nil {
			m.reads = append(m.reads, *c.RecordRead)
		}
	}
}

func (m *contextReplay) cliCompaction(seq sessionstore.Sequence, at time.Time, key ContextDimensions, v llm.ConversationRecord) {
	if v.Phase != "compact" && !v.Compacted && !v.Interrupted && v.Error == "" {
		return
	}
	key.Strategy = historicalSummaryStrategy
	if v.Provider != "" {
		key.Provider = viewer.SafeText(v.Provider)
	}
	if v.Model != "" {
		key.Model = viewer.SafeText(v.Model)
	}
	c := &contextCompactionLog{key: key, at: at, engine: "CLI", outcome: "completed", numbers: make(contextCounts)}
	switch {
	case v.BeforeSource != "":
		measureInput(c.numbers, "before_tokens", v.BeforeTokens, v.BeforeSource)
	case v.BeforeTokens > 0:
		source := "measured"
		if !v.Compacted || v.Usage != nil {
			source = "derived" // legacy fallback to the last reported input
		}
		c.numbers.add("before_tokens", float64(v.BeforeTokens), source)
	}
	if v.AfterKnown || v.AfterTokens > 0 {
		c.numbers.tokens("cli_post_tokens", &v.AfterTokens, "measured")
	}
	c.numbers.tokens("summary_output_tokens", v.SummaryOutputTokens, "measured")
	if v.SummaryOutputTokens == nil && v.Usage != nil && v.Usage.OutputTokens > 0 && !slices.Contains(v.Usage.Unknown, llm.UsageOutput) {
		c.numbers.add("summary_output_tokens", float64(v.Usage.OutputTokens), "measured")
	}
	if !v.StartedAt.IsZero() && !v.EndedAt.IsZero() {
		source := "derived"
		if v.DurationSource == "provider_api" {
			source = "measured"
		}
		c.numbers.merge("generation_seconds", interval(v.StartedAt, v.EndedAt, source))
	}
	if v.Interrupted {
		c.outcome = "interrupted"
	}
	m.addCompaction(seq, c, v.Compacted, v.DurationSource != "")
}

func (m *contextReplay) addCompaction(seq sessionstore.Sequence, c *contextCompactionLog, completed, instrumented bool) {
	if completed {
		w := m.startWindow(c.at, seq, c.key, "compaction")
		w.compaction, w.l3Covered = c, instrumented
	} else if c.outcome != "interrupted" {
		c.outcome = "failed"
	}
	c.contextPosition = m.position(seq)
	m.compactions = append(m.compactions, c)
}

func measureInput(c contextCounts, metric string, value int64, source string) {
	switch source {
	case "provider-reported":
		c.tokens(metric, &value, "measured")
	case "estimated", "measured-prefix+estimated-delta", "last-reported-input":
		c.tokens(metric, &value, "derived")
	default:
		c.unknown(metric)
	}
}

func (m *contextReplay) response(seq sessionstore.Sequence, at time.Time, v sessionstore.ModelResponse, request RequestMetric) {
	t := m.turns[v.TurnID]
	if t == nil {
		// Some legacy histories only have the public response. The work interval
		// will remain unknown because its request start is missing.
		t = &contextTurnLog{window: m.window(), after: seq}
		m.turns[v.TurnID] = t
		m.invocations = append(m.invocations, t)
	}
	t.end, t.hasReply = at, true
	if request.Provider != "" {
		t.key.Provider = request.Provider
	}
	if request.Model != "" {
		t.key.Model = request.Model
	}
	if request.Effort != "" {
		t.key.Effort = request.Effort
	}
	if request.Fast != "" {
		t.key.Fast = request.Fast
	}
	if v.WindowCache.Boundary != "" {
		t.firstCache = v.WindowCache
	}
	if !isClaude(t.key.Provider) && !request.toolBatch && t.latest == nil && t.single == nil {
		t.single = &contextRequest{
			contextPosition: contextPosition{after: t.after, window: t.window}, key: t.key,
			at: t.at, usage: responseRequestUsage(v.Response.Usage),
			countKnown: v.Response.ID != "" || llm.ReportedTokens(v.Response.Usage).InputKnown,
		}
		if t.single.at.IsZero() {
			t.single.at = at
		}
		m.requests = append(m.requests, t.single)
	}
	p := contextPosition{after: seq, window: t.window, turn: t, request: t.latest}
	if t.latest != nil {
		p.window = t.latest.window
	}
	m.questions = append(m.questions, contextQuestionCount{p, userQuestions(v.Response.Output)})
}

func isClaude(provider string) bool {
	return provider == "claude" || provider == "claude-code" || provider == "claude-api" || provider == "claude-code-api"
}

func userQuestions(items []llm.Item) int {
	n := 0
	for _, item := range items {
		msg, ok := item.Data.(llm.Message)
		if !ok || item.Type != llm.ItemMessage || msg.Role != "" && msg.Role != llm.RoleAssistant {
			continue
		}
		switch msg.Phase {
		case "", "commentary", "final", "final_answer":
			first, _ := utf8.DecodeRuneInString(strings.TrimLeftFunc(msg.Text, unicode.IsSpace))
			if first != '【' && !displaytext.UnsafeRune(first) {
				continue
			}
			if _, ok := displaytext.Attention(displaytext.Message(msg.Text)); ok {
				n++
			}
		}
	}
	return n
}

func (m *contextReplay) activity(seq sessionstore.Sequence, at time.Time, key ContextDimensions, a llm.ActivityRecord) {
	switch a.Kind {
	case "child_start":
		m.childStart(a.ID, seq)
	case "claude_stream":
		m.stream(seq, at, key, a)
	case llm.ActivitySubscriptionUsage:
		if t := m.turns[session.TurnID(a.ID)]; t != nil {
			key = t.key
		}
		key.Strategy = m.window().key.Strategy
		if a.Provider != "" {
			key.Provider = viewer.SafeText(a.Provider)
		}
		if a.Model != "" {
			key.Model = viewer.SafeText(a.Model)
		}
		if a.Effort != "" {
			key.Effort = viewer.SafeText(a.Effort)
		}
		key.Fast = fastLabel(a.Fast)
		for _, usage := range a.Subscription {
			m.subscription = append(m.subscription, reportSubscription(m.id, key, at, usage))
		}
	}
}

func (m *contextReplay) stream(seq sessionstore.Sequence, at time.Time, key ContextDimensions, a llm.ActivityRecord) {
	var event struct {
		Event           string `json:"event"`
		ParentToolUseID string `json:"parent_tool_use_id"`
		Compaction      bool   `json:"compaction"`
		Output          *int64 `json:"output_tokens"`
	}
	if json.Unmarshal([]byte(a.Reason), &event) != nil || event.ParentToolUseID != "" || event.Compaction || a.Model == "<synthetic>" {
		return
	}
	turnID, _, ok := strings.Cut(a.ID, "/stream/")
	t := m.turns[session.TurnID(turnID)]
	if !ok || t == nil {
		return
	}
	switch event.Event {
	case "message_start":
		if m.streams[a.ID] != nil {
			return
		}
		key = t.key
		key.Strategy = m.window().key.Strategy
		if a.Provider != "" {
			key.Provider = viewer.SafeText(a.Provider)
		}
		if a.Model != "" {
			key.Model = viewer.SafeText(a.Model)
		}
		r := &contextRequest{contextPosition: contextPosition{after: seq, window: m.window()}, key: key, at: a.StartedAt, countKnown: true}
		if r.at.IsZero() {
			r.at = at
		}
		if a.RequestUsage != nil {
			r.usage = *a.RequestUsage
		}
		m.streams[a.ID], t.latest, m.currentReq = r, r, r
		m.requests = append(m.requests, r)
		m.point(seq, t)
	case "message_stop":
		if r := m.streams[a.ID]; r != nil {
			if a.RequestUsage != nil {
				r.usage.Output = a.RequestUsage.Output
			} else if event.Output != nil && *event.Output > 0 {
				r.usage.Output = event.Output
			}
		}
	}
}

func responseRequestUsage(u llm.Usage) llm.RequestUsage {
	var r llm.RequestUsage
	t := llm.ReportedTokens(u)
	var raw struct {
		Input   *int64 `json:"input_tokens"`
		Output  *int64 `json:"output_tokens"`
		Details struct {
			CachedTokens     *int64 `json:"cached_tokens"`
			CacheWriteTokens *int64 `json:"cache_write_tokens"`
		} `json:"input_tokens_details"`
	}
	_ = json.Unmarshal(u.Raw, &raw)
	if t.InputKnown && (t.Input > 0 || raw.Input != nil) {
		r.Input = new(t.Input)
	}
	if t.OutputKnown && (t.Output > 0 || raw.Output != nil) {
		r.Output = new(t.Output)
	}
	if !slices.Contains(u.Unknown, llm.UsageCachedInput) && (u.CachedInputTokens > 0 || raw.Details.CachedTokens != nil) {
		r.Read = new(u.CachedInputTokens)
	}
	if !slices.Contains(u.Unknown, llm.UsageCacheWriteInput) && (u.CacheWriteInputTokens > 0 || raw.Details.CacheWriteTokens != nil) {
		r.Write = new(u.CacheWriteInputTokens)
	}
	return r
}

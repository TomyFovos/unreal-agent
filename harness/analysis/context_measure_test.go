package analysis

import (
	"encoding/json/v2"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/contextstate"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/record"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

var measurementEpoch = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

type measurementFixture struct {
	*Accumulator
	seq sessionstore.Sequence
}

func (f *measurementFixture) add(second int, data any) {
	f.seq++
	f.Apply(host.HistoryItem{Sequence: f.seq, RecordedAt: measurementEpoch.Add(time.Duration(second) * time.Second), Data: data})
}

func (f *measurementFixture) contextRecord(second int, c sessionstore.ContextRecord) {
	f.add(second, sessionstore.HostRecord{Kind: sessionstore.HostContext, Context: &c})
}

func (f *measurementFixture) activity(second int, a llm.ActivityRecord) {
	f.add(second, sessionstore.HostRecord{Kind: sessionstore.HostActivity, Activity: &a})
}

func (f *measurementFixture) turn(second int, id string, strategy string) {
	f.add(second, session.Turn{ID: session.TurnID(id), Type: session.TurnRegular, ContextStrategy: strategy})
}

func (f *measurementFixture) respond(second int, id, provider, model, message string, u llm.Usage) {
	f.add(second, sessionstore.ModelResponse{TurnID: session.TurnID(id), Response: llm.Response{
		ID: id, Provider: provider, Model: model, Usage: u,
		Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: message}}},
	}})
}

func measuredNumber(t *testing.T, numbers ContextNumbers, field string, want float64, source string, missing int) {
	t.Helper()
	n := numbers[field]
	if n.Value == nil || math.Abs(*n.Value-want) > 1e-9 || n.Source != source || n.Missing != missing {
		t.Fatalf("%s = %s, want %g (%s, %d unknown)", field, contextNumberText(n, ""), want, source, missing)
	}
}

func unknownNumber(t *testing.T, numbers ContextNumbers, field string) {
	t.Helper()
	n := numbers[field]
	if n.Value != nil || contextNumberText(n, "") != "unknown" {
		t.Fatalf("%s must remain unknown: %+v", field, n)
	}
}

func reportGroup(t *testing.T, groups []ContextGroup, strategy, model, fast string) ContextNumbers {
	t.Helper()
	for _, g := range groups {
		if g.Strategy == strategy && g.Model == model && g.Fast == fast {
			return g.Numbers
		}
	}
	t.Fatalf("missing group %s / %s / %s", strategy, model, fast)
	return nil
}

func TestContextMeasurementsWindowFallback(t *testing.T) {
	f := &measurementFixture{Accumulator: New("main")}
	f.contextRecord(1, sessionstore.ContextRecord{Kind: sessionstore.ContextWindowFallback, Provider: "claude-code", Model: "opus", Window: &sessionstore.WindowBoundary{
		Fallback: sessionstore.WindowFallbackL2OverBudget, BeforeTokens: 195_000, BeforeMeasurement: "estimated",
		HeaderTokens: new(int64(8_000)), L2Tokens: 190_000, L2TargetTokens: 5_000,
		Tail: &sessionstore.WindowTail{Tokens: 5_000}, AfterTokens: 203_000, InputBudgetTokens: 200_000,
	}})
	f.turn(2, "summary", "summary")
	r := (&Tree{nodes: []*treeNode{{acc: f.Accumulator, observed: true}}}).Snapshot(nil).ContextMeasurements
	fallbacks := r.Sessions[0].Fallbacks
	if len(fallbacks) != 1 || fallbacks[0].Reason != sessionstore.WindowFallbackL2OverBudget || fallbacks[0].Strategy != historicalSummaryStrategy {
		t.Fatalf("fallback observation = %+v", fallbacks)
	}
	n := fallbacks[0].Numbers
	measuredNumber(t, n, "required_tokens", 203_000, "derived", 0)
	measuredNumber(t, n, "header_tokens", 8_000, "derived", 0)
	measuredNumber(t, n, "l2_tokens", 190_000, "derived", 0)
	measuredNumber(t, n, "tail_tokens", 5_000, "derived", 0)
	measuredNumber(t, n, "input_budget_tokens", 200_000, "measured", 0)
	lines := strings.Join(contextLines(r), "\n")
	if !strings.Contains(lines, "Historical fallback: l2_over_budget") || !strings.Contains(lines, "203000 tokens") {
		t.Fatal("fallback reason and sizes are missing from /analyze")
	}
}

func TestContextMeasurementsExcerptedFit(t *testing.T) {
	f := &measurementFixture{Accumulator: New("main")}
	f.add(0, sessionstore.HostRecord{Kind: sessionstore.HostRuntimeApplied, Selection: &sessionstore.RuntimeSelection{
		Provider: "openai-codex", Model: "fixture",
	}})
	f.contextRecord(1, sessionstore.ContextRecord{Kind: sessionstore.ContextWindowBoundary, Provider: "openai-codex", Model: "fixture", Window: &sessionstore.WindowBoundary{
		Fit: sessionstore.WindowFitExcerpted, FitExcerptChars: new(120), InputBudgetTokens: 4_000,
		RequiredInputTokens: 10_000, RequiredTailTokens: 9_000, Estimator: "fixture", AfterTokens: 3_999,
		HeaderTokens: new(int64(400)), L2Tokens: 600, Tail: &sessionstore.WindowTail{Tokens: 2_999, Items: 2},
	}})
	f.turn(2, "fitted", contextengine.WindowThreeLayer)
	r := f.ContextReport(true)
	if groups := r.Sessions[0].Groups; len(groups) != 1 || groups[0].Strategy != contextengine.WindowThreeLayer {
		t.Fatal("first rebuild created a second strategy group", groups)
	}
	n := reportGroup(t, r.Sessions[0].Windows[0].Groups, contextengine.WindowThreeLayer, "fixture", "unknown")
	measuredNumber(t, n, "fit_excerpted", 1, "measured", 0)
	measuredNumber(t, n, "fit_excerpt_chars", 120, "measured", 0)
	measuredNumber(t, n, "fit_required_input", 10_000, "derived", 0)
	measuredNumber(t, n, "fit_required_tail", 9_000, "derived", 0)
	measuredNumber(t, n, "fit_input_budget", 4_000, "measured", 0)
	if !strings.Contains(strings.Join(contextLines(r), "\n"), "Fit: excerpted") {
		t.Fatal("/analyze omitted the fitted boundary")
	}
}

func TestContextMeasurementsWindowsAndExport(t *testing.T) {
	f := &measurementFixture{Accumulator: New("main")}
	const strategy = contextengine.WindowThreeLayer
	const provider, model = "opencode-go", "qwen3.8-flash"
	f.add(1, sessionstore.HostRecord{Kind: "configuration", Configuration: []byte(`{"Provider":{"provider":"opencode-go","model":{"id":"qwen3.8-flash"}},"ReasoningEffort":"high"}`)})
	w := sessionstore.WindowBoundary{ThresholdTokens: 200000, L2TargetTokens: 5000, RecentEventLimit: new(8), ToolOutputExcerptChars: 128}
	f.contextRecord(2, sessionstore.ContextRecord{Kind: sessionstore.ContextWindowStart, Provider: provider, Model: model, Window: &w})
	off := false
	f.turn(10, "initial", strategy)
	f.respond(20, "initial", provider, model, "Normal reply.", llm.Usage{InputTokens: 500, CachedInputTokens: 50, Fast: &off})
	state := contextstate.State{
		QuestionsToUser: contextstate.Entries[contextstate.Question]{Current: map[string]contextstate.Question{"q": {ID: "q", FirstLine: "private-question-sensitive"}}},
		Approvals:       contextstate.Entries[contextstate.Approval]{Current: map[string]contextstate.Approval{"a": {ID: "a"}}},
	}
	raw, err := state.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	b := w
	b.Causes = []string{"threshold", "admission"}
	b.BeforeTokens, b.BeforeMeasurement, b.AfterTokens, b.L2Tokens = 200500, "provider-reported", 18000, 5100
	b.Estimator, b.RecentEvents = "fixture", 7
	b.HeaderTokens = new(int64(2200))
	prev := []record.Locator{record.Item("main", 4)}
	b.Tail = &sessionstore.WindowTail{Tokens: 10000, Items: 4, Previous: &prev}
	f.contextRecord(30, sessionstore.ContextRecord{Kind: sessionstore.ContextWindowBoundary, Provider: provider, Model: model, Text: "snapshot\n" + string(raw), Window: &b})
	firstBoundary := f.seq
	f.turn(40, "rewarm", strategy)
	f.respond(50, "rewarm", provider, model, "\x1b[31m【要確認】\x1b[0m First fixture question.", llm.Usage{
		InputTokens: 1500, CacheWriteInputTokens: 100, OutputTokens: 50, Fast: &off,
		Raw: []byte(`{"input_tokens":1500,"output_tokens":50,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":100}}`),
	})
	f.contextRecord(52, sessionstore.ContextRecord{Kind: "record_read", RecordRead: &sessionstore.RecordRead{Action: "show", After: f.seq, Bytes: new(int64(25)), PreviousTailHits: new(int64(1))}})
	f.contextRecord(53, sessionstore.ContextRecord{Kind: "record_read", RecordRead: &sessionstore.RecordRead{Action: "find", After: f.seq, Bytes: new(int64(12)), PreviousTailHits: new(int64(0))}})
	e, err := contextstate.NewEvent("note:decision", contextstate.DecisionRecorded, contextstate.Decision{ID: "decision", Decision: "private-decision-sensitive"})
	if err != nil {
		t.Fatal(err)
	}
	f.add(54, e)
	f.turn(60, "warm", strategy)
	f.respond(70, "warm", provider, model, "【要確認】 Second fixture question.", llm.Usage{
		InputTokens: 1000, CachedInputTokens: 800, OutputTokens: 100, Fast: &off,
		Raw: []byte(`{"input_tokens":1000,"output_tokens":100,"input_tokens_details":{"cached_tokens":800,"cache_write_tokens":0}}`),
	})
	second := b
	second.Causes = []string{"header_change", "mid_turn"}
	second.BeforeTokens, second.BeforeMeasurement, second.AfterTokens, second.L2Tokens = 202000, "estimated", 20000, 5200
	second.Tail = nil // old boundary format did not measure its tail
	second.HeaderTokens = nil
	f.contextRecord(90, sessionstore.ContextRecord{Kind: sessionstore.ContextWindowBoundary, Provider: provider, Model: model, Window: &second})
	f.turn(100, "no-usage", strategy)
	f.respond(110, "no-usage", provider, model, "Normal reply.", llm.Usage{Fast: &off})
	f.activity(111, llm.ActivityRecord{Kind: llm.ActivitySubscriptionUsage, ID: "no-usage", Provider: provider, Model: model, Fast: &off,
		Subscription: []llm.SubscriptionUsage{{Window: "primary", UsedPercent: new(0.0), ResetsAt: measurementEpoch.Add(time.Hour)}}})
	facts := &sessionstore.ContextFacts{Through: f.seq, Rows: []sessionstore.ContextFactCounts{
		{After: firstBoundary, Events: map[contextstate.EventKind]int64{contextstate.WindowRebuilt: 1}},
	}}
	project := (&Tree{nodes: []*treeNode{{acc: f.Accumulator, observed: true, facts: facts}}}).Snapshot(nil)
	metrics := project.ContextMeasurements
	s := metrics.Sessions[0]
	if metrics.Partial || len(s.Windows) != 3 || len(s.Subscription) != 1 {
		t.Fatalf("wrong context coverage: partial=%v windows=%d subscription=%d", metrics.Partial, len(s.Windows), len(s.Subscription))
	}
	r := reportGroup(t, s.Groups, strategy, model, "off")
	for _, reason := range []string{"threshold", "over_budget", "header_change", "mid_turn"} {
		measuredNumber(t, r, "reason_"+reason, 1, "measured", 0)
	}
	measuredNumber(t, r, "boundaries", 2, "measured", 0)
	measuredNumber(t, r, "boundary_interval_requests", 2, "measured", 1)
	measuredNumber(t, r, "boundary_interval_seconds", 60, "derived", 1)
	measuredNumber(t, r, "tail_tokens", 10000, "derived", 1)
	measuredNumber(t, r, "window_header_tokens", 2200, "derived", 1)
	measuredNumber(t, r, "required_minimum_over_target", 0, "measured", 1)
	measuredNumber(t, r, "user_questions", 2, "measured", 0)
	measuredNumber(t, r, "work_seconds", 40, "derived", 0)
	measuredNumber(t, s.Questions[0].Numbers, "questions_per_hour", 180, "derived", 0)
	measuredNumber(t, metrics.Totals.Questions[0].Numbers, "questions_per_hour", 180, "derived", 0)
	measuredNumber(t, r, "within_hit_percent", 100.0*850/1500, "derived", 0)
	measuredNumber(t, r, "first_cache_read", 0, "measured", 1)
	measuredNumber(t, r, "state_decide", 1, "measured", 0)
	measuredNumber(t, r, "machine_window_rebuilt", 1, "derived", 0)

	first := reportGroup(t, s.Windows[1].Groups, strategy, model, "off")
	measuredNumber(t, first, "boundary_before_tokens", 200500, "measured", 0)
	measuredNumber(t, first, "boundary_after_tokens", 18000, "derived", 0)
	measuredNumber(t, first, "l2_copy_tokens", 5100, "derived", 0)
	measuredNumber(t, first, "l2_target_tokens", 5000, "measured", 0)
	measuredNumber(t, first, "recent_events", 7, "measured", 0)
	measuredNumber(t, first, "pending_approvals", 1, "derived", 0)
	measuredNumber(t, first, "pending_user_questions", 1, "derived", 0)
	measuredNumber(t, first, "first_cache_write", 100, "measured", 0)
	measuredNumber(t, first, "first_uncached", 1400, "derived", 0)
	measuredNumber(t, first, "first_usd_write", 100*.2/1e6, "derived", 0)
	measuredNumber(t, first, "first_usd_total", (1400*.15+100*.2+50*.47)/1e6, "derived", 0)
	measuredNumber(t, first, "within_hit_percent", 80, "derived", 0)
	measuredNumber(t, first, "record_show_bytes", 25, "measured", 0)
	measuredNumber(t, first, "record_find_bytes", 12, "measured", 0)
	measuredNumber(t, first, "record_previous_tail_calls", 1, "derived", 0)
	measuredNumber(t, first, "record_previous_tail_percent", 50, "derived", 0)
	unknown := reportGroup(t, s.Windows[2].Groups, strategy, model, "off")
	unknownNumber(t, unknown, "first_input")
	unknownNumber(t, unknown, "tail_items")
	unknownNumber(t, unknown, "pending_approvals")
	unknownNumber(t, unknown, "required_minimum_over_target")
	if s.Subscription[0].UsedPercent.Value == nil || *s.Subscription[0].UsedPercent.Value != 0 || s.Subscription[0].WindowMinutes.Value != nil {
		t.Fatal("subscription zero or missing field lost", s.Subscription)
	}

	report := Report{Version: 1, Session: "main", Tree: project}
	lines := strings.Join(Lines(report, "Context metrics", true), "\n")
	for _, want := range []string{"over_budget  1 (measured)", "First request cache read  0 tokens (measured, 1 unknown)", "Within-window cache hit ratio  80%", "Questions per work hour  180", "Subscription utilisation"} {
		if !strings.Contains(lines, want) {
			t.Fatalf("missing TUI metric %q", want)
		}
	}
	jsonData, err := Encode(report, "JSON")
	if err != nil {
		t.Fatal(err)
	}
	var restored struct {
		Tree struct{ ContextMeasurements ContextMeasurements }
	}
	if err := json.Unmarshal(jsonData, &restored); err != nil {
		t.Fatal(err)
	}
	before, err := json.Marshal(metrics, json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	after, err := json.Marshal(restored.Tree.ContextMeasurements, json.Deterministic(true))
	if err != nil || !slices.Equal(before, after) {
		t.Fatal("context measurements changed on a JSON round trip", err)
	}
	md, err := Encode(report, "Markdown")
	if err != nil || !strings.Contains(string(md), "| Within-window cache hit ratio | 80% (derived) |") || !strings.Contains(string(md), "| L1 rendered items | unknown |") {
		t.Fatal("Markdown context tables missing or mislabelled", err)
	}
	for _, data := range []string{lines, string(jsonData), string(md)} {
		if strings.Contains(data, "-sensitive") || strings.Contains(data, string(prev[0])) {
			t.Fatal("report exported evidence bodies or reread locators")
		}
	}
}

func TestHistoricalSummaryShowsOldCLIAndUnrealCompactions(t *testing.T) {
	f := &measurementFixture{Accumulator: New("summary")}
	const strategy = historicalSummaryStrategy
	f.add(1, sessionstore.HostRecord{Kind: "configuration", Configuration: []byte(`{"Provider":{"provider":"claude-code","model":{"id":"fixture-opus"}},"ReasoningEffort":"max"}`)})
	f.turn(5, "cli", "summary")
	stream := func(second int, id, model, scope string, usage *llm.RequestUsage) {
		raw, err := json.Marshal(map[string]string{"event": "message_start", "parent_tool_use_id": scope})
		if err != nil {
			t.Fatal(err)
		}
		f.activity(second, llm.ActivityRecord{Kind: "claude_stream", ID: "cli/stream/" + id, Provider: "claude-code", Model: model, Reason: string(raw), RequestUsage: usage})
	}
	usage := func(uncached, read, write int64) *llm.RequestUsage {
		return &llm.RequestUsage{Input: new(uncached + read + write), Uncached: &uncached, Read: &read, Write: &write}
	}
	stream(6, "1", "fixture-opus", "", usage(100, 0, 5))
	stream(7, "2", "fixture-native-child", "tool-1", usage(1000000, 0, 0))
	f.contextRecord(8, sessionstore.ContextRecord{Kind: "provider_conversation", Conversation: &llm.ConversationRecord{
		Provider: "claude-code", Model: "fixture-opus", Phase: "compact", Compacted: true, BeforeTokens: 200000, BeforeSource: "provider-reported",
		AfterKnown: true, StartedAt: measurementEpoch.Add(6 * time.Second), EndedAt: measurementEpoch.Add(8 * time.Second), DurationSource: "provider_api",
	}})
	stream(9, "3", "fixture-opus", "", usage(40, 60, 0))
	stream(10, "4", "fixture-opus", "", usage(10, 85, 5))
	f.respond(12, "cli", "claude-code", "fixture-opus", "【要確認】 Summary fixture question.", llm.Usage{InputTokens: 1000005, CachedInputTokens: 900000})
	f.add(13, sessionstore.HostRecord{Kind: sessionstore.HostRuntimeApplied, Selection: &sessionstore.RuntimeSelection{Provider: "opencode-go", Model: "qwen3.8-flash"}})
	summary := llm.Usage{InputTokens: 15000, OutputTokens: 500}
	f.contextRecord(14, sessionstore.ContextRecord{Kind: "summary", Provider: "opencode-go", Model: "qwen3.8-flash", Usage: &summary,
		Generation: &llm.GenerationStatistics{Source: "client_clock", Tokens: llm.ReportedTokens(summary), DurationMicros: 1500000}})
	f.turn(15, "post-summary", "summary")
	f.respond(18, "post-summary", "opencode-go", "qwen3.8-flash", "Done.", llm.Usage{InputTokens: 700})
	s := f.ContextReport(true).Sessions[0]
	if len(s.Compactions) != 2 {
		t.Fatal("missing CLI/Unreal compactions", s.Compactions)
	}
	lines := strings.Join(contextLines(f.ContextReport(true)), "\n")
	for _, label := range []string{"Historical CLI compaction", "Historical Unreal compaction", historicalSummaryStrategy} {
		if !strings.Contains(lines, label) {
			t.Fatalf("/analyze is missing the historical label %q", label)
		}
	}
	cli := s.Compactions[0]
	if cli.Strategy != historicalSummaryStrategy || s.Compactions[1].Strategy != historicalSummaryStrategy {
		t.Fatal("old compactions were reported as an active strategy")
	}
	measuredNumber(t, cli.Numbers, "before_tokens", 200000, "measured", 0)
	measuredNumber(t, cli.Numbers, "cli_post_tokens", 0, "measured", 0)
	measuredNumber(t, cli.Numbers, "post_request_input_tokens", 100, "measured", 0)
	measuredNumber(t, cli.Numbers, "generation_seconds", 2, "measured", 0)
	unknownNumber(t, cli.Numbers, "summary_output_tokens")
	unreal := s.Compactions[1]
	measuredNumber(t, unreal.Numbers, "before_tokens", 15000, "measured", 0)
	measuredNumber(t, unreal.Numbers, "post_request_input_tokens", 700, "measured", 0)
	measuredNumber(t, unreal.Numbers, "summary_output_tokens", 500, "measured", 0)
	measuredNumber(t, unreal.Numbers, "generation_seconds", 1.5, "derived", 0)

	first := reportGroup(t, s.Windows[1].Groups, strategy, "fixture-opus", "unknown")
	measuredNumber(t, first, "first_requests", 1, "measured", 0)
	measuredNumber(t, first, "first_cache_read", 60, "measured", 0)
	measuredNumber(t, first, "first_uncached", 40, "measured", 0)
	measuredNumber(t, first, "within_requests", 1, "measured", 0)
	measuredNumber(t, first, "within_hit_percent", 85, "derived", 0)
	unknownNumber(t, first, "first_usd_total")
	measuredNumber(t, s.Questions[0].Numbers, "questions_per_hour", 360, "derived", 0)
	for _, g := range s.Groups {
		if g.Model == "fixture-native-child" {
			t.Fatal("included an untracked native Claude CLI child in main request counts")
		}
	}
}

func TestContextMeasurementsSeparateFastCohorts(t *testing.T) {
	f := &measurementFixture{Accumulator: New("fast")}
	for i, tc := range []struct {
		id   string
		fast bool
		in   int64
	}{{"standard", false, 100}, {"fast", true, 200}} {
		f.turn(2*i+1, tc.id, contextengine.WindowThreeLayer)
		f.respond(2*i+2, tc.id, "openai-codex", "fixture", "Done.", llm.Usage{Fast: &tc.fast, InputTokens: tc.in})
	}
	groups := f.ContextReport(true).Sessions[0].Groups
	for _, tc := range []struct {
		mode string
		in   float64
	}{{"off", 100}, {"on", 200}} {
		n := reportGroup(t, groups, contextengine.WindowThreeLayer, "fixture", tc.mode)
		measuredNumber(t, n, "within_requests", 1, "measured", 0)
		measuredNumber(t, n, "within_input", tc.in, "measured", 0)
		unknownNumber(t, n, "within_uncached")
		unknownNumber(t, n, "within_hit_percent")
	}
}

package analysis

import (
	"encoding/json/v2"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/contextstate"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
)

func TestHourlyTokensSeparatesReadEMAAndIncompleteHour(t *testing.T) {
	now := time.Unix(1800000000, 0).Truncate(time.Hour).Add(30 * time.Minute)
	var samples []UsageSample
	for i, n := range []int64{100, 200, 999} {
		samples = append(samples, UsageSample{At: now.Add(time.Duration(i-2) * time.Hour), Usage: viewer.Usage{Known: true, Input: n + 20, CachedInput: 20, CacheWriteInput: 7, Output: 5}})
	}
	series, read := HourlyTokens(samples, now.Add(-3*time.Hour), now, true, nil)
	if n := len(series.Hourly); n != 12 || series.Hourly[n-1].Value != 1004 || series.Hourly[0].Known || read[n-1].Value != 20 {
		t.Fatal("hourly inputs, including current hour, are wrong", series, read)
	}
	ema := 105*(1-EMAAlpha) + 205*EMAAlpha
	if n := len(series.Projection); n != TokenForecastHours+2 || series.Projection[n-1].Value != ema {
		t.Fatal("forecast included the incomplete hour or charged cache reads", series.Projection)
	}
	samples[1].Usage.Unknown = []llm.UsageField{llm.UsageCachedInput}
	series, read = HourlyTokens(samples, now.Add(-3*time.Hour), now, true, nil)
	if series.Hourly[10].Known || read[10].Known || len(series.Projection) != 0 {
		t.Fatal("a missing cache counter became zero or fed the EMA", series, read)
	}
	var mix TokenMix
	mix.Add(viewer.Usage{Known: true, Input: 100, CachedInput: 80, Output: 10, Unknown: []llm.UsageField{llm.UsageCacheWriteInput}})
	if !mix.Weighted.Known || mix.Weighted.Value != 30 || mix.Write.Known || mix.Uncached.Known || mix.Cache.Percent().Value != 80 {
		t.Fatal("cache write presence should not change the weighted ranking", mix)
	}
	if !mix.UnsplitInput.Known || mix.UnsplitInput.Value != 20 {
		t.Fatal("non-read input lost when the write counter is unknown", mix)
	}
}

func TestAllowanceForecastUsesIrregularEMAWithinResetCycle(t *testing.T) {
	t0 := time.Unix(1800000000, 0)
	at := t0.Add(2 * time.Hour)
	reset := at.Add(2 * time.Hour)
	window := SubscriptionWindow{SubscriptionObservation: llm.SubscriptionObservation{Provider: "claude", ObservedAt: at, SubscriptionUsage: llm.SubscriptionUsage{Window: "five_hour", UsedPercent: new(70.0), ResetsAt: reset}}}
	observation := func(t time.Time, value float64, r time.Time) ContextSubscription {
		return ContextSubscription{ContextDimensions: ContextDimensions{ModelKey: ModelKey{Provider: "claude"}}, At: t, Window: "five_hour", UsedPercent: ContextNumber{Value: &value}, ResetsAt: &r}
	}
	history := []ContextSubscription{
		observation(t0.Add(-time.Minute), 99, t0),
		observation(t0, 30, reset),
		observation(t0.Add(30*time.Minute), 35, reset),
		observation(t0.Add(30*time.Minute), 40, reset),
	}
	r := Report{ContextMeasurements: &ContextMeasurements{Totals: ContextScope{Subscription: history}}}
	f := r.ForecastAllowance(window)
	if !f.Rate.Known || math.Abs(f.Rate.Value-20) > 1e-9 || len(f.History) != 3 || len(f.Projection) != 2 || !f.ExhaustedAt.Equal(at.Add(90*time.Minute)) {
		t.Fatalf("forecast includes an old window or a duplicate sample: %+v", f)
	}
	*window.UsedPercent = 55
	f = r.ForecastAllowance(window)
	firstWeight := (1 - math.Pow(1-EMAAlpha, .5)) * math.Pow(1-EMAAlpha, 1.5)
	secondWeight := 1 - math.Pow(1-EMAAlpha, 1.5)
	want := (20*firstWeight + 10*secondWeight) / (firstWeight + secondWeight)
	if math.Abs(f.Rate.Value-want) > 1e-9 || !f.ExhaustedAt.After(reset) || !f.Projection[1].At.Equal(reset) {
		t.Fatalf("irregular rate smoothing / reset clipping: %+v", f)
	}
	*window.UsedPercent = 5
	f = r.ForecastAllowance(window)
	if f.Rate.Known || !f.ExhaustedAt.IsZero() || len(f.History) != 1 || len(f.Projection) != 0 {
		t.Fatalf("forecast crosses a reset or treats a single sample as zero rate: %+v", f)
	}
	window.UsedPercent = nil
	if f = r.ForecastAllowance(window); f.Rate.Known || len(f.History) != 0 || len(f.Projection) != 0 {
		t.Fatalf("unreported allowance fabricated an observation: %+v", f)
	}
}

func TestAllowanceForecastWeightsElapsedTimeAndMatchesJitteredReset(t *testing.T) {
	start := time.Date(2026, 11, 23, 1, 56, 0, 0, time.UTC)
	at := start.Add(122 * time.Minute)
	reset := at.Add(19 * time.Hour)
	window := SubscriptionWindow{SubscriptionObservation: llm.SubscriptionObservation{
		Provider: "openai-codex", ObservedAt: at.Add(time.Minute), PercentObservedAt: at,
		SubscriptionUsage: llm.SubscriptionUsage{Window: "primary", WindowMinutes: new(int64(10080)), ResetsAt: reset, UsedPercent: new(92.0), Status: "rejected"},
	}}
	observation := func(at time.Time, used, period float64, reset time.Time) ContextSubscription {
		return ContextSubscription{ContextDimensions: ContextDimensions{ModelKey: ModelKey{Provider: "openai-codex"}},
			At: at, Window: "primary", WindowMinutes: ContextNumber{Value: &period}, UsedPercent: ContextNumber{Value: &used}, ResetsAt: &reset}
	}
	history := []ContextSubscription{observation(start, 90, 10080, reset.Add(-time.Second))}
	for i := 1; i <= 120; i++ {
		used := 91.0
		if i < 3 {
			used = 90
		}
		if i == 120 {
			used = 92
		}
		history = append(history, observation(start.Add(time.Duration(i)*time.Minute), used, 10080, reset.Add(time.Duration(i%2)*time.Second)))
	}
	history = append(history, observation(at.Add(-time.Second), 10, 300, reset))
	r := Report{ContextMeasurements: &ContextMeasurements{Totals: ContextScope{Subscription: history}}}
	f := r.ForecastAllowance(window)
	if len(f.History) != 122 || !f.History[len(f.History)-1].At.Equal(at) || f.Rate.Value < 1.02 || f.Rate.Value > 1.05 ||
		!f.ExhaustedAt.Before(reset) || len(f.Projection) != 2 || !f.Projection[1].At.Equal(f.ExhaustedAt) {
		t.Fatalf("expected both reset timestamps, the same period and roughly 1.03 pp/h: %+v", f)
	}
}

func TestDashboardKeepsPerModelSplitAndDoesNotDoubleCountClaude(t *testing.T) {
	a := New("main")
	t0 := time.Unix(1800000000, 0)
	a.Created(t0)
	for i, sample := range []struct {
		kind, model string
		in          int64
	}{
		{"claude_usage", "opus", 100},
		{"provider_usage", "opus", 60},
		{"provider_usage", "sonnet", 40},
	} {
		a.Apply(host.HistoryItem{Sequence: sessionstore.Sequence(i + 1), RecordedAt: t0, Data: sessionstore.HostRecord{Kind: sessionstore.HostActivity, Activity: &llm.ActivityRecord{
			Kind: sample.kind, Provider: "claude-code", ID: "turn", Model: sample.model, EndedAt: t0,
			Usage: &llm.Usage{InputTokens: sample.in},
		}}})
	}
	a.Apply(host.HistoryItem{Sequence: 4, RecordedAt: t0, Data: sessionstore.ModelResponse{TurnID: "turn", Response: llm.Response{Model: "opus", Usage: llm.Usage{InputTokens: 100}}}})
	a.Apply(host.HistoryItem{Sequence: 5, RecordedAt: t0, Data: sessionstore.ModelResponse{TurnID: "next", Response: llm.Response{Model: "opus", Usage: llm.Usage{InputTokens: 20}}}})
	r := a.Snapshot(t0, false, false, nil, nil)
	a.DashboardReport(&r, nil)
	if len(r.Dashboard.Usage) != 3 || r.Usage.Input != 120 || r.Dashboard.Usage[0].Usage.Input != 60 || r.Dashboard.Usage[1].Model != "sonnet" || r.Dashboard.Usage[2].Usage.Input != 20 {
		t.Fatal("incorrect attribution of reported usage", r.Usage, r.Dashboard.Usage)
	}
}

func TestDashboardReconcilesLegacyBreakdownsAndClaudeToolBatches(t *testing.T) {
	a := New("main")
	t0 := time.Unix(1800000000, 0).UTC()
	add := func(data any) { a.Apply(host.HistoryItem{RecordedAt: t0, Data: data}) }
	const model = "claude-opus-5-5"
	total := func(kind, model string, input, read int64) {
		add(sessionstore.HostRecord{Kind: sessionstore.HostActivity, Activity: &llm.ActivityRecord{
			Kind: kind, ID: "exec", Provider: "claude", Model: model, EndedAt: t0,
			Usage: &llm.Usage{InputTokens: input, CachedInputTokens: read},
		}})
	}
	response := func(turn session.TurnID, input, read int64, batch bool) {
		reply := llm.Response{Model: model, Usage: llm.Usage{InputTokens: input, CachedInputTokens: read}}
		if batch {
			reply.Output = []llm.Item{{Type: llm.ItemToolCall, Data: llm.ToolCall{Name: tool.ClaudeCodeName, CallID: string(turn)}}}
		}
		add(sessionstore.ModelResponse{TurnID: turn, Response: reply})
	}
	add(session.Turn{ID: "exec"})
	response("exec", 60, 57, true)
	add(session.Turn{ID: "continued", ToolContinuation: "exec"})
	total("claude_usage", model, 100, 88)
	total("provider_usage", model, -80, 1)
	total("provider_usage", "claude-sonnet-5-5", 180, 87)
	response("continued", 40, 38, false)
	add(session.Turn{ID: "old"})
	response("old", 10, 7, false)
	add(session.Turn{ID: "interrupted"})
	response("interrupted", 500, 499, true)
	r := a.Snapshot(t0, false, false, nil, nil)
	a.DashboardReport(&r, nil)

	var mix TokenMix
	for _, sample := range r.Dashboard.Usage {
		if sample.Provider != "claude" || sample.Model != model {
			t.Fatalf("unreconciled model row: %+v", sample)
		}
		mix.Add(sample.Usage)
	}
	if len(r.Dashboard.Usage) != 3 || mix.Input.Value != 110 || mix.Read.Value != 95 || mix.Cache.Percent().Value != 100*95.0/110 ||
		mix.Cache.Percent().Unknown != 1 {
		t.Fatalf("execution, legacy final, and partial inputs did not reconcile: %+v %+v", mix, r.Dashboard.Usage)
	}
}

func TestDashboardContextReadsFirstBoundarySampleAndLegacyHistory(t *testing.T) {
	a := New("main")
	t0 := time.Unix(1800000000, 0)
	appendItem := func(after int, data any) {
		a.Apply(host.HistoryItem{Sequence: sessionstore.Sequence(after), RecordedAt: t0.Add(time.Duration(after) * time.Minute), Data: data})
	}
	appendItem(1, sessionstore.HostRecord{Kind: sessionstore.HostContext, Context: &sessionstore.ContextRecord{Kind: "prune"}})
	appendItem(2, sessionstore.HostRecord{Kind: sessionstore.HostContext, Context: &sessionstore.ContextRecord{Kind: sessionstore.ContextWindowBoundary,
		Window: &sessionstore.WindowBoundary{Causes: []string{"threshold", "mid_turn"}, ThresholdTokens: 1000, Estimator: "fixture", L2Tokens: 200, Tail: &sessionstore.WindowTail{Tokens: 100}}}})
	for i, input := range []struct {
		id          session.TurnID
		read, write int64
	}{{"after", 700, 100}, {"steady", 950, 10}} {
		appendItem(3+i*2, session.Turn{ID: input.id, ContextStrategy: "three-layer"})
		appendItem(4+i*2, sessionstore.ModelResponse{TurnID: input.id, Response: llm.Response{Usage: llm.Usage{InputTokens: 1000, CachedInputTokens: input.read, CacheWriteInputTokens: input.write}}})
	}
	c := a.context.dashboard(a.pruning.Epochs)
	if len(c.Requests) != 2 || len(c.Boundaries) != 1 || c.Threshold.Value != 1000 || c.L1.Value != 100 || c.L2.Value != 200 ||
		c.FirstRead.Value != 70 || c.SteadyRead.Value != 95 || c.FirstWrites.Value != 100 || c.OtherWrites.Value != 10 || c.Legacy != 1 {
		t.Fatalf("context measurements use the wrong samples: %+v", c)
	}
}

func TestWallTimePartitionsOverlapsAndRecordedApproval(t *testing.T) {
	t0 := time.Unix(1800000000, 0)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	a := New("s")
	a.Created(t0)
	for i, decision := range []string{"", "allow"} {
		a.Apply(host.HistoryItem{Sequence: sessionstore.Sequence(i + 1), RecordedAt: at(25 + i*10), Data: sessionstore.HostRecord{
			Kind: sessionstore.HostApproval, Approval: &permission.Approval{Request: permission.Pending{ID: "p"}, Decision: decision},
		}})
	}
	a.Apply(host.HistoryItem{Sequence: 3, RecordedAt: at(90), Data: sessionstore.HostRecord{
		Kind: sessionstore.HostApproval, Approval: &permission.Approval{Request: permission.Pending{ID: "unclosed"}},
	}})
	wall := a.wallSignal(nil)
	wall.HasTurns = true
	wall.add(TimeModel, at(10), at(40))
	wall.add(TimeTools, at(20), at(60))
	wall.add(TimeChildren, at(50), at(80))
	wall.add(TimeLimit, at(35), at(50))
	wall.add(TimeUnattributed, at(80), at(90))
	totals := SpanTotals(wall.Partition(at(100), true), t0, at(100))
	want := [TimeKinds]time.Duration{20, 10, 10, 5, 30, 15, 10}
	for i := range want {
		want[i] *= time.Second
	}
	if totals != want {
		t.Fatal("overlap double-counted or residual silently assigned", totals, want)
	}
	wall.IncludeLiveWaits(false, []permission.Pending{{ID: "unclosed"}})
	live := SpanTotals(wall.Partition(at(100), true), t0, at(100))
	if live[TimeApproval] != 20*time.Second || live[TimeIdle] != 10*time.Second {
		t.Fatal("pending live approval was not included", live)
	}
}

func TestWallRecognizesShellOperationAndNativeChildWait(t *testing.T) {
	t0 := time.Unix(1800000000, 0).UTC()
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	a := New("s")
	a.Created(t0)
	add := func(s int, data any) { a.Apply(host.HistoryItem{RecordedAt: at(s), Data: data}) }
	args, err := json.Marshal(map[string]any{"tool": "Bash", "input": map[string]string{
		"command": "cd /work; U='/path with spaces/unreal-agent'; \"$U\" subagent wait --timeout 30s",
	}})
	if err != nil {
		t.Fatal(err)
	}
	native := llm.ToolCall{Name: tool.ClaudeCodeName, CallID: "native", Arguments: string(args)}
	bridge := llm.ToolCall{Name: tool.BashName, CallID: "shell", Arguments: "{}"}
	for _, call := range []llm.ToolCall{native, bridge} {
		add(5, sessionstore.ModelResponse{TurnID: "run", Response: llm.Response{
			Output: []llm.Item{{Type: llm.ItemToolCall, Data: call}},
		}})
	}
	add(12, sessionstore.HostRecord{Kind: sessionstore.HostActivity, Activity: &llm.ActivityRecord{
		Kind: "native_bash", Tool: "Bash", ID: "native", StartedAt: at(7), EndedAt: at(12),
	}})
	add(13, sessionstore.HostRecord{Kind: sessionstore.HostActivity, Activity: &llm.ActivityRecord{
		Kind: "native_bash", Tool: "Bash", ID: "pending", StartedAt: at(13),
	}})
	spec, err := operation.NewShellSpec(operation.ShellInput{Command: "unreal-agent subagent wait", Shell: "/bin/bash", Directory: t.TempDir()}, t.TempDir(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	op := operation.Operation{ID: "wait", Type: spec.Type, Version: spec.Version, State: spec.State, MaxOutputLength: spec.MaxOutputLength, Status: operation.StatusReady}
	status := sessionstore.ToolCallStatus{TurnID: "run", CallID: "shell", Status: tool.CallStatus{WaitingFor: []operation.ID{op.ID}}, Operations: []operation.Operation{op}}
	add(10, status)
	op.Status = operation.StatusCompleted
	status.Operations = []operation.Operation{op}
	add(20, status)
	add(45, sessionstore.HostRecord{Kind: sessionstore.HostActivity, Activity: &llm.ActivityRecord{Kind: "model_runtime"}})
	r := a.Snapshot(at(50), false, false, nil, nil)
	a.DashboardReport(&r, nil)
	byResult := make(map[string]ToolSample)
	for _, sample := range r.Dashboard.Tools {
		byResult[sample.Result] = sample
	}
	if len(byResult) != 3 || !byResult["unknown"].ChildWait || !byResult["completed"].ChildWait || !byResult[""].End.IsZero() {
		t.Fatalf("incorrect child waits or tool terminality: %+v", r.Dashboard.Tools)
	}
	wall := r.Dashboard.Wall
	totals := SpanTotals(wall.Partition(wall.LastRecord, true), wall.Start, wall.LastRecord)
	if !wall.End.IsZero() || !wall.LastRecord.Equal(at(45)) || totals[TimeChildren] != 15*time.Second || totals[TimeTools] != 0 {
		t.Fatalf("wall time lost a known wait or double counted an overlap: %+v, %v", wall, totals)
	}
	if childWaitCommand("echo 'pretend; unreal-agent subagent wait'") || childWaitCommand("echo unreal-agent subagent wait") {
		t.Fatal("an argument was classified as a wait")
	}
}

func TestQuestionHeadingIsPresentInTimelineAndOmittedFromExport(t *testing.T) {
	a := New("s")
	q, err := contextstate.NewEvent("note:fixture", contextstate.QuestionChanged, contextstate.Question{
		Status: contextstate.QuestionOpen, FirstLine: "【要確認】次の確認は必要ですか？",
	})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Unix(1800000000, 0).UTC()
	a.Apply(host.HistoryItem{RecordedAt: at, Data: q})
	r := a.Snapshot(at, false, false, nil, nil)
	if len(r.Timeline) != 1 || r.Timeline[0].Kind != "question_to_user" || r.Timeline[0].Name != "open" ||
		r.Timeline[0].Question != "【要確認】次の確認は必要ですか？" {
		t.Fatal("question event missing", r.Timeline)
	}
	raw, err := json.Marshal(r)
	if err != nil || strings.Contains(string(raw), r.Timeline[0].Question) {
		t.Fatal("question heading entered metadata export", err, string(raw))
	}
}

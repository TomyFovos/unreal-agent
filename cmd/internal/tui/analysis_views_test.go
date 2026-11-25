package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rivo/uniseg"
	"github.com/unreallabsai/unreal-agent/harness/analysis"
	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
)

func analysisText(r analysis.Report, view string, width int, raw bool) string {
	rows := analysisRows(r, analysisTarget{view: view, width: width, raw: raw}, Theme{Plain: true})
	lines := make([]string, len(rows))
	for i, row := range rows {
		lines[i] = row.plain()
	}
	return strings.Join(lines, "\n")
}

func TestAnalysisSurfacesCausesOutliersAndFullModels(t *testing.T) {
	r := analysisFixture()
	r.Dashboard.Context.Boundaries[0].Causes = []string{"header_change", "provider_overflow", "future_cause", "header_change"}
	for _, cause := range []string{"threshold", "header_change", "provider_overflow", "future_cause"} {
		content := analysisText(r, "Context", 99, false)
		for _, row := range strings.Split(content, "\n") {
			if strings.HasPrefix(row, cause) && !strings.HasSuffix(row, "1 / 2") {
				t.Fatalf("boundary cause counted more than once: %q", row)
			}
		}
		if !strings.Contains(content, cause) {
			t.Fatalf("boundary cause missing: %s", cause)
		}
	}
	model := "deepseek-v4.1-flash"
	usage := viewer.Usage{Known: true, Input: 479300, CachedInput: 239000, Output: 500, Unknown: []llm.UsageField{llm.UsageCacheWriteInput}}
	r.Turns = append(r.Turns, analysis.Turn{Number: 1402, ObservedModel: model, Usage: usage})
	turns := strings.Split(analysisText(r, "Turns", 99, false), "\n")
	if len(turns) <= 2 || !strings.Contains(turns[2], "#1402 "+model) {
		t.Fatalf("largest input should be the first turn: %q", turns[:min(len(turns), 3)])
	}
	r.Dashboard.Children[0].Title = strings.Repeat("長い日本語タイトル", 10)
	r.Dashboard.Children[0].Model = model
	r.Dashboard.Usage[0].Session = r.Dashboard.Children[0].ID
	r.Dashboard.Usage[0].Model = model
	r.Dashboard.Usage[0].Usage = usage
	for _, check := range []struct{ view, text string }{
		{"Usage", "…  " + model}, {"Usage", "W —  U —  N 240.3k"}, {"Agents", " worker     " + model},
	} {
		if got := analysisText(r, check.view, 99, false); !strings.Contains(got, check.text) {
			t.Errorf("%s missing %q\n%s", check.view, check.text, got)
		}
	}
	if got := analysisText(r, "Agents", 99, false); !strings.Contains(got, "… ") || strings.Contains(got, "…worker") {
		t.Errorf("title needs an ellipsis and a column separator:\n%s", got)
	}
	if text := analysisText(r, "Overview", 99, false); strings.Contains(text, "Cache read/h") || !strings.Contains(text, "Cache read 1h") {
		t.Fatal("overview should retain the last-hour share", text)
	}
}

func TestAnalysisContextShowsActivePolicyAndPeriodMetrics(t *testing.T) {
	r := analysisFixture()
	old := sessionstore.WindowPolicy{WindowThresholdTokens: 200_000, PostRebuildRatio: 0.5, L2TargetTokens: 5000, ToolOutputExcerptChars: 128}
	current := sessionstore.WindowPolicy{WindowThresholdTokens: 180_000, PostRebuildRatio: 0.6, L2TargetTokens: 4000}
	from, changed := r.At.Add(-time.Hour), r.At
	r.Tree = &analysis.TreeReport{ContextMeasurements: &analysis.ContextMeasurements{
		ActivePolicies: []analysis.ContextRoutePolicy{{Provider: "claude", Policy: current}, {Provider: "opencode-go", Role: "lead", Policy: old}},
		Sessions: []analysis.ContextSession{{Session: "lead", PolicyPeriods: []analysis.ContextPolicyPeriod{
			{Provider: "opencode-go", Role: "lead", From: &from, Policy: &old},
		}}, {Session: "main", PolicyPeriods: []analysis.ContextPolicyPeriod{
			{Provider: "claude", From: &from, Until: &changed, Policy: &old, Numbers: analysis.ContextNumbers{
				"rebuilds": {Value: new(2.0)}, "boundary_after_tokens": {Value: new(24_000.0)},
				"first_cache_read_share": {Value: new(68.0)}, "record_show_calls": {Value: new(5.0)}, "record_find_calls": {Value: new(1.0)},
			}},
			{Provider: "claude", From: &changed, Policy: &current, Numbers: analysis.ContextNumbers{"rebuilds": {Value: new(0.0)}}},
		}}},
	}}
	got := analysisText(r, "Context", 105, false)
	for _, want := range []string{"Latest applied policy by route", "claude  W 180000 · ×0.6 · L2 4000",
		"opencode-go / lead  W 200000", "opencode-go / lead  " + displayTime(from, "01/02 15:04:05"), "excerpt 128 chars",
		"Rebuilds by policy period", "W 200000 · ×0.5 · L2 5000", "Rebuilds 2 · mean after 24.0k tokens · first cache read 68.0%",
		"L3 show 5 / find 1", "Rebuilds 0 · mean after — tokens"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
}

func TestAnalysisShowsKnownFailuresAndUnknownToolResults(t *testing.T) {
	r := analysisFixture()
	r.Dashboard.Failures = append(r.Dashboard.Failures, analysis.Event{Kind: "child", Name: "undated failure"})
	r.Dashboard.Tools = append(r.Dashboard.Tools, analysis.ToolSample{Name: "Bash", Result: "unknown", Start: r.At.Add(-time.Minute), End: r.At},
		analysis.ToolSample{Name: "Bash", Start: r.At})
	if got := analysisText(r, "Errors", 99, false); !strings.Contains(got, "~1 / 2") {
		t.Fatal("an undated child failure hid a known last-hour failure", got)
	}
	if got := analysisText(r, "Tools", 99, false); !strings.Contains(got, "1 result unknown  1 incomplete") {
		t.Fatal("tool end with unknown result counted as incomplete", got)
	}
}

func TestAnalysisTimeUsesLastRecordForStoppedSessionAndShowsQuestions(t *testing.T) {
	r := analysisFixture()
	r.Dashboard.State = "stopped"
	r.Dashboard.Wall.Start = r.At.Add(-2 * time.Hour)
	r.Dashboard.Wall.LastRecord = r.At.Add(-time.Hour)
	r.Timeline = append(r.Timeline, analysis.Event{At: r.At, Kind: "question_to_user", Name: "answered",
		Question: "どの検証を先に行いますか？ " + strings.Repeat("追加の説明", 25)})
	if got := analysisText(r, "Time", 99, false); !strings.Contains(got, "1h00m  100%") || strings.Contains(got, "2h00m  100%") {
		t.Fatal("stopped time did not end at the last record", got)
	}
	if got := analysisText(r, "Time", 99, true); !strings.Contains(got, "question_to_user  answered  どの検証を先に行いますか？") ||
		len(strings.Split(got, "\n")) <= 4 {
		t.Fatal("expanded time did not retain and wrap the question heading", got)
	}
}

func TestAnalysisChartAggregatesColumnsAndPlacesGuideAndReset(t *testing.T) {
	first := time.Unix(1800000000, 0).UTC()
	next := first.Add(time.Hour)
	reset := next.Add(time.Hour)
	points := []analysis.Sample{{At: first, Known: true, Value: 239700}, {At: next, Known: true, Value: 70000}}
	g := graph{height: 5, format: tokensText, limit: analysis.Metric{Value: 200000, Known: true}, markers: []time.Time{reset},
		includeMarkers: true, series: []chartSeries{{points: points, st: liveStyle, mark: '#'}}}
	render := func(g graph, ascii bool) []string {
		p := analysisPainter{width: 99, theme: Theme{ASCII: ascii}}
		p.chart(g)
		out := make([]string, len(p.rows))
		for i, l := range p.rows {
			out[i] = l.plain()
		}
		return out
	}
	for _, ascii := range []bool{false, true} {
		base := render(g, ascii)
		mark := "┬"
		if ascii {
			mark = "+"
		}
		if len(base) != 7 || !strings.Contains(base[0], "239.7k") || strings.Contains(base[0], "W") ||
			!strings.Contains(base[1], "200.0k W") || !strings.HasSuffix(base[5], mark) {
			t.Fatalf("guide overwrote max tick or the reset fell off the axis: %q", base)
		}
		dense := make([]analysis.Sample, 0, 1001)
		dense = append(dense, points[0])
		for i := 1; i < 1000; i++ {
			dense = append(dense, analysis.Sample{At: first.Add(time.Duration(i) * time.Millisecond), Known: true, Value: float64(i)})
		}
		testGraph := g
		testGraph.series = []chartSeries{{points: append(dense, points[1]), st: liveStyle, mark: '#'}}
		if got := render(testGraph, ascii); strings.Join(got, "\n") != strings.Join(base, "\n") {
			t.Fatalf("samples hidden by the peak in one column changed the plot: %q\n%q", got, base)
		}
	}
}

func analysisFixture() analysis.Report {
	clock := time.Date(2026, time.November, 23, 14, 40, 0, 0, time.Local)
	start := clock.Add(-12*time.Hour - 40*time.Minute)
	gpt := analysis.ModelKey{Provider: "openai-codex", Model: "gpt-6.2-sol", Effort: "max"}
	claude := analysis.ModelKey{Provider: "claude-code", Model: "claude-opus-5-5", Effort: "high"}
	m := func(v float64) analysis.Metric { return analysis.Metric{Known: true, Value: v} }
	d := &analysis.Dashboard{State: "running", BoundaryWrites: m(18000), OtherWrites: m(8200)}
	r := analysis.Report{Session: "fixture", At: clock, CreatedAt: start, Dashboard: d, ContextPackage: &contextengine.Diagnostics{Excluded: 2, CanonicalUnits: 80}}
	d.Children = []analysis.ChildSignal{
		{ID: "build", Title: "Build release package", Role: "worker", State: "running", ModelKey: gpt, Start: clock.Add(-time.Hour), LastActivity: clock.Add(-20 * time.Minute)},
		{ID: "audit", Title: "Review export path", Role: "lead", State: "ended", Result: "failed", ModelKey: claude, Start: clock.Add(-3 * time.Hour), End: clock.Add(-20 * time.Minute)},
		{ID: "scan", Title: "Collect format fixtures", Role: "collector", State: "running", ModelKey: claude, Start: clock.Add(-time.Hour), LastActivity: clock.Add(-49 * time.Minute)},
	}
	d.Context = analysis.ContextSignal{L1: m(28400), L2: m(7900), Threshold: m(120000), FirstRead: m(64), SteadyRead: m(94)}
	for i, weighted := range []int64{9400, 12600, 6300, 17000, 24400, 4300, 21700, 18900, 6300, 9700, 16400, 9200} {
		hour := start.Add(time.Duration(i+1)*time.Hour + 12*time.Minute)
		usage := viewer.Usage{Known: true, Input: 54000 + weighted, CachedInput: 54000, CacheWriteInput: weighted / 10, Output: weighted / 2}
		d.Usage = append(d.Usage, analysis.UsageSample{Session: "fixture", Role: "main", ModelKey: gpt, At: hour, Usage: usage})
		r.Turns = append(r.Turns, analysis.Turn{Number: i + 1, At: hour, ObservedModel: gpt.Model, Usage: usage})
		d.Context.Requests = append(d.Context.Requests, analysis.Sample{At: hour, Value: float64([]int{21, 42, 57, 100, 131, 27, 39, 109, 125, 35, 76, 113}[i]) * 1000, Known: true})
		d.ProviderOK = append(d.ProviderOK, hour)
	}
	d.Context.Boundaries = []analysis.BoundarySignal{
		{At: start.Add(5*time.Hour + 55*time.Minute), Causes: []string{"threshold", "mid_turn"}},
		{At: start.Add(9*time.Hour + 55*time.Minute), Causes: []string{"threshold", "admission"}},
	}
	for i, child := range d.Children {
		in := int64(180000 - i*51000)
		d.Usage = append(d.Usage, analysis.UsageSample{Session: child.ID, Role: child.Role, ModelKey: child.ModelKey, At: clock.Add(-time.Duration(i+1) * 19 * time.Minute),
			Usage: viewer.Usage{Known: true, Input: in, CachedInput: in / 10 * 9, CacheWriteInput: in / 100, Output: in / 50}})
	}
	d.Tools = []analysis.ToolSample{
		{Name: "Bash", Result: "completed", Start: clock.Add(-30 * time.Minute), End: clock.Add(-25 * time.Minute)},
		{Name: "Bash", Result: "failed", Start: clock.Add(-7 * time.Minute), End: clock.Add(-6 * time.Minute)},
		{Name: "Read", Result: "completed", Start: clock.Add(-58 * time.Minute), End: clock.Add(-57 * time.Minute)},
	}
	d.Failures = []analysis.Event{
		{At: clock.Add(-95 * time.Minute), Kind: "tool", Name: "Bash"},
		{At: clock.Add(-6 * time.Minute), Kind: "tool", Name: "Bash"},
		{At: clock.Add(-20 * time.Minute), Kind: "child", Name: "Review export path"},
		{At: clock.Add(-96 * time.Minute), Kind: "provider retry", Name: "Transport timeout"},
	}
	r.Timeline = []analysis.Event{{At: clock.Add(-25 * time.Minute), Kind: "tool status", Name: "Bash completed"}}
	d.Wall = analysis.WallSignal{Start: start, HasTurns: true, Spans: []analysis.TimeSpan{
		{Start: clock.Add(-65 * time.Minute), End: clock.Add(-5 * time.Minute), Kind: analysis.TimeModel},
		{Start: clock.Add(-30 * time.Minute), End: clock.Add(-25 * time.Minute), Kind: analysis.TimeTools},
		{Start: clock.Add(-45 * time.Minute), End: clock.Add(-40 * time.Minute), Kind: analysis.TimeChildren},
		{Start: clock.Add(-65 * time.Minute), End: clock.Add(-61 * time.Minute), Kind: analysis.TimeLimit},
		{Start: clock.Add(-43 * time.Minute), End: clock.Add(-39 * time.Minute), Kind: analysis.TimeApproval},
		{Start: clock.Add(-5 * time.Minute), End: clock.Add(-2 * time.Minute), Kind: analysis.TimeUnattributed},
	}}
	recordAllowance := func(provider, window string, used []float64, reset time.Time) {
		times := []time.Time{clock.Add(-121 * time.Minute), clock.Add(-61 * time.Minute), clock.Add(-time.Minute)}
		r.SubscriptionUsage = append(r.SubscriptionUsage, analysis.SubscriptionWindow{State: "observed", SubscriptionObservation: llm.SubscriptionObservation{
			Provider: provider, ObservedAt: times[2], SubscriptionUsage: llm.SubscriptionUsage{Window: window, UsedPercent: &used[2], ResetsAt: reset},
		}})
		for i := range times {
			if r.ContextMeasurements == nil {
				r.ContextMeasurements = &analysis.ContextMeasurements{}
			}
			r.ContextMeasurements.Totals.Subscription = append(r.ContextMeasurements.Totals.Subscription, analysis.ContextSubscription{
				ContextDimensions: analysis.ContextDimensions{ModelKey: analysis.ModelKey{Provider: provider}},
				Window:            window, At: times[i], UsedPercent: analysis.ContextNumber{Value: &used[i]}, ResetsAt: &reset,
			})
		}
	}
	recordAllowance("openai-codex", "primary", []float64{64, 73, 82.5}, clock.Add(time.Hour))
	recordAllowance("claude", "five_hour", []float64{70, 82, 94}, clock.Add(3*time.Hour))
	r.SubscriptionUsage = append(r.SubscriptionUsage, analysis.SubscriptionWindow{State: "unknown", SubscriptionObservation: llm.SubscriptionObservation{Provider: "claude", SubscriptionUsage: llm.SubscriptionUsage{Window: "seven_day"}}})
	return r
}

func TestAnalysisChartsAllViewsAt80And160(t *testing.T) {
	r := analysisFixture()
	want := map[string][]string{
		"Overview":                     {"Cache read 1h", "Next 3h", "Remaining allowance", "Active children", "Failures"},
		"Usage":                        {"By model", "By role", "Top children", "Cache writes", "Hourly weighted tokens", "collector", "Build release package"},
		analysis.SubscriptionUsageView: {"17.5%", "6.0%", "(before reset)", "(after reset)", "pp/h", "Observed ", "—"},
		"Context":                      {"120.0k", "threshold", "mid_turn", "First", "Steady", "L3 —", "Security exclusions 2 / 80"},
		"Agents":                       {"Build release package", "Review export path", "collector", "49m", "Failed"},
		"Tools":                        {"Bash", "Failure rate", "50.0%", "Slowest calls", "5m00s"},
		"Time":                         {"Approval", "Waiting for children", "Model generation", "Per hour"},
		"Errors":                       {"Failures", "Recent failures", "Transport timeout", "Review export path"},
		"Turns":                        {"relative to largest turn", "#12", "R 54.0k"},
	}
	for _, view := range analysis.Views[:len(analysis.Views)-1] {
		for _, width := range []int{80, 160} {
			for _, ascii := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%d/ascii=%t", view, width, ascii), func(t *testing.T) {
					theme := Theme{Plain: true, ASCII: ascii}
					p := readyAnalysis(r, view, theme, width)
					f := RenderFrame(Snapshot{}, UIState{analysis: p, Theme: theme}, width, 24)
					if len(f.Lines) != 24 || f.Lines[0] != analysisQuestion(view, false) || strings.Contains(strings.Join(f.Lines, "\n"), "Ask") {
						t.Fatal("question or whole-screen viewport missing", f)
					}
					content := make([]string, len(p.rows))
					for i, row := range p.rows {
						content[i] = row.plain()
						if uniseg.StringWidth(content[i]) >= width || strings.ContainsAny(content[i], "\n\r") {
							t.Fatalf("chart line broke the right edge: %q", content[i])
						}
						for _, ch := range content[i] {
							graphGlyph := ch >= 0x2800 && ch <= 0x28FF || strings.ContainsRune("█▮▰▁▂▃▄▅▆▇░▒▓─│┬└", ch)
							if ascii && graphGlyph {
								t.Fatalf("ASCII output used %q", ch)
							}
						}
					}
					text := strings.Join(content, "\n")
					if width == 80 && !ascii {
						height := 24
						if view == "Usage" {
							height = 40
						} else if view == "Context" || view == analysis.SubscriptionUsageView {
							height = 26
						}
						capture := RenderFrame(Snapshot{}, UIState{analysis: p, Theme: theme}, width, height)
						t.Log("\n" + strings.Join(capture.Lines, "\n"))
					}
					for _, s := range want[view] {
						if !strings.Contains(text, s) {
							t.Fatalf("missing %q\n%s", s, text)
						}
					}
					if view == "Overview" || view == "Usage" || view == "Context" || view == analysis.SubscriptionUsageView {
						if ascii && !strings.Contains(text, "-----") || !ascii && !strings.ContainsFunc(text, func(r rune) bool { return r > 0x2800 && r < 0x2900 }) {
							t.Fatal("missing multi-row chart", text)
						}
					}
					if ascii && view == analysis.SubscriptionUsageView && !strings.Contains(text, ".  .") {
						t.Fatal("missing dotted forecast", text)
					}
				})
			}
		}
	}
}

func TestAnalysisExportQuestionAndChoices(t *testing.T) {
	for _, width := range []int{80, 160} {
		for _, ascii := range []bool{false, true} {
			p := &analysisScreen{picker: analyzePicker("Export")}
			p.key(Key{Name: "enter"}, Frame{})
			f := RenderFrame(Snapshot{}, UIState{analysis: p, Theme: Theme{ASCII: ascii, Plain: true}}, width, 24)
			text := strings.Join(f.Lines, "\n")
			if strings.TrimSpace(f.Lines[0]) != analysisQuestion("Export", false) || !strings.Contains(text, "JSON") || !strings.Contains(text, "Markdown") {
				t.Fatal("export choices or question missing", text)
			}
			p.key(Key{Name: "escape"}, f)
			if p.picker.Options[p.picker.Selection] != "Export" {
				t.Fatal("Export Escape did not return to the view list")
			}
		}
	}
}

func TestAnalysisThresholdStyles(t *testing.T) {
	for _, tc := range []struct {
		name   string
		metric func(analysis.Metric) style
		values [4]float64
	}{
		{"cache", cacheStyle, [4]float64{74.99, 75, 89.99, 90}},
		{"allowance", allowanceStyle, [4]float64{10, 10.01, 20, 20.01}},
		{"failures", rateStyle, [4]float64{15, 14.99, 5, 4.99}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for i, v := range tc.values {
				st := tc.metric(analysis.Metric{Known: true, Value: v})
				want := [4]style{failureStyle, warningStyle, warningStyle, successStyle}[i]
				if st != want || !strings.Contains(textLine("value", st).paint(Theme{}), (Theme{}).sgr(want)) {
					t.Fatal("threshold did not use the Theme state", v, st, want)
				}
			}
			if tc.metric(analysis.Metric{}) != meta {
				t.Fatal("unknown received a threshold color")
			}
		})
	}
	for i, age := range []time.Duration{15 * time.Minute, 15*time.Minute + time.Second, 45 * time.Minute, 45*time.Minute + time.Second} {
		if staleStyle(age) != [4]style{successStyle, warningStyle, warningStyle, failureStyle}[i] {
			t.Fatal("stuck threshold", age)
		}
	}
}

func TestAnalysisUnknownValuesAndLegacyJapanese(t *testing.T) {
	r := analysis.Report{Dashboard: &analysis.Dashboard{Context: analysis.ContextSignal{Legacy: 2}}}
	data := analysisRows(r, analysisTarget{view: "Context", width: 79, ja: true}, Theme{ASCII: true, Plain: true})
	var lines []string
	for _, line := range data {
		lines = append(lines, line.plain())
	}
	text := strings.Join(lines, "\n")
	for _, s := range []string{"旧要約方式の記録 2 件", "L1 —  L2 —  L3 —", "W —"} {
		if !strings.Contains(text, s) {
			t.Fatal("legacy data was obscured", text)
		}
	}
	for _, s := range []string{"0.0%", "0 / 0", "Estimator", "Canonical context units", "prune"} {
		if strings.Contains(text, s) {
			t.Fatal("display fabricated or leaked legacy details", text)
		}
	}
	p := &analysisScreen{view: "Context", ja: true, report: &r, rendered: analysisTarget{view: "Context", width: 79, ja: true}, rows: data}
	f := RenderFrame(Snapshot{}, UIState{analysis: p}, 80, 24)
	if f.Lines[0] != textLine(analysisQuestion("Context", true), strong).paint(Theme{}) {
		t.Fatal("Japanese question missing", f.Lines[0])
	}
	p2 := readyAnalysis(r, "Usage", Theme{Plain: true}, 80)
	if p2.rows[3].plain() != absentMeasure {
		t.Fatal("empty usage represented a measured zero", p2.rows)
	}
}

func TestAnalysisTimeRawExpansionAndArrowPageScroll(t *testing.T) {
	r := analysisFixture()
	p := readyAnalysis(r, "Time", Theme{Plain: true}, 80)
	ui := UIState{analysis: p, Theme: Theme{Plain: true}}
	f := RenderFrame(Snapshot{}, ui, 80, 14)
	p.key(Key{Name: "down"}, f)
	if p.offset != 1 {
		t.Fatal("down does not scroll analysis by one row")
	}
	p.key(Key{Name: "pagedown"}, f)
	if p.offset != f.SheetHeight+1 {
		t.Fatal("Page Down does not scroll by the viewport", p.offset)
	}
	p.key(Key{Name: "pageup"}, f)
	p.key(Key{Name: "up"}, f)
	if p.offset != 0 {
		t.Fatal("scrolling back does not reach the question")
	}
	if strings.Contains(strings.Join(f.Lines, "\n"), "Bash completed") {
		t.Fatal("raw events leaked into default Time")
	}
	p.key(Key{Text: "e"}, f)
	p.rendered.raw = true
	p.rows = analysisRows(r, p.rendered, ui.Theme)
	text := strings.Join(RenderFrame(Snapshot{}, ui, 80, 14).Lines, "\n")
	if !p.raw || !strings.Contains(text, "Bash completed") || strings.Contains(text, "Per hour") {
		t.Fatal("raw event expansion not reachable", text)
	}
}

func TestAnalyzeReturnsToChatDuringBlockedHistoryRead(t *testing.T) {
	t.Setenv("LC_ALL", "C.UTF-8")
	f := newFake()
	f.view.Session.Session.ID = "s"
	keys := make(chan Key, 8)
	done := make(chan error, 1)
	entered := make(chan struct{}, 1)
	ended := make(chan struct{}, 1)
	out := &screenObserver{}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		done <- Run(ctx, Config{ID: "s", Client: f, Keys: keys, Output: out, Theme: &Theme{Plain: true},
			AnalysisInspect: func(ctx context.Context, _ session.ID, _ sessionstore.Sequence, _ int) (host.View, error) {
				entered <- struct{}{}
				<-ctx.Done()
				ended <- struct{}{}
				return host.View{}, ctx.Err()
			},
		})
	}()
	waitFor(t, func() bool { return strings.Contains(out.text(), "Ask") })
	keys <- Key{Text: "/analyze"}
	keys <- Key{Name: "enter"}
	waitFor(t, func() bool { return strings.Contains(out.text(), "Overview") })
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("analysis did not start reading")
	}
	keys <- Key{Name: "enter"}
	waitFor(t, func() bool {
		return strings.Contains(out.text(), "Reading analysis") && strings.Contains(out.text(), "Is this Session healthy") && !strings.Contains(out.text(), "Ask")
	})
	keys <- Key{Name: "escape"}
	waitFor(t, func() bool { return strings.Contains(out.text(), "Enter open") })
	keys <- Key{Name: "escape"}
	waitFor(t, func() bool { return strings.Contains(out.text(), "Ask") })
	select {
	case <-ended:
	case <-time.After(3 * time.Second):
		t.Fatal("closing the pager did not cancel the pending read")
	}
	keys <- Key{Name: "detach"}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("TUI did not detach")
	}
}

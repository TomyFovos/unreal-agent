package tui

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/analysis"
	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
	"github.com/unreallabsai/unreal-agent/internal/testsession"
)

func TestSubscriptionUsageAcrossSessionTree(t *testing.T) {
	dir := t.TempDir()
	f := testsession.WriteWorkerTierTree(t, dir)
	store, err := localfile.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	shortReset, weekReset := now.Add(3*time.Hour), now.AddDate(0, 0, 4)
	opusReset := now.Add(time.Minute)
	appendUsage := func(id session.ID, provider string, windows ...llm.SubscriptionUsage) time.Time {
		t.Helper()
		item, err := store.AppendHostRecordRecorded(t.Context(), id, sessionstore.HostRecord{
			Version: 1, Kind: sessionstore.HostActivity, Activity: &llm.ActivityRecord{
				Kind: llm.ActivitySubscriptionUsage, Provider: provider, Subscription: windows,
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return item.RecordedAt
	}
	weekAt := appendUsage(f.Lead, "claude",
		llm.SubscriptionUsage{Window: "five_hour", UsedPercent: new(25.0), ResetsAt: shortReset},
		llm.SubscriptionUsage{Window: "seven_day", Status: "allowed_warning", UsedPercent: new(31.0), ResetsAt: weekReset})
	shortAt := appendUsage("main", "claude", llm.SubscriptionUsage{Window: "five_hour", Status: "allowed", UsedPercent: new(71.0), ResetsAt: shortReset})
	opusAt := appendUsage("main", "claude", llm.SubscriptionUsage{Window: "seven_day_opus", Status: "rejected", UsedPercent: new(89.0), ResetsAt: opusReset})
	appendUsage("main", "claude", llm.SubscriptionUsage{Window: "overage", Status: "allowed"})
	zeroAt := appendUsage("main", "openai-codex",
		llm.SubscriptionUsage{Window: "primary", UsedPercent: new(41.0), WindowMinutes: new(int64(10080)), ResetsAt: weekReset},
		llm.SubscriptionUsage{Window: "secondary", UsedPercent: new(0.0), WindowMinutes: new(int64(300)), ResetsAt: shortReset})
	archived := f.Workers["qwen-archived"]
	codexAt := appendUsage(archived, "openai-codex", llm.SubscriptionUsage{Window: "primary", UsedPercent: new(74.0), WindowMinutes: new(int64(10080)), ResetsAt: weekReset})
	if moved, err := store.ArchiveChild(t.Context(), localfile.ArchiveEntry{ChildID: archived, ParentID: f.Lead, OperationID: "qwen-archived", Status: operation.StatusCompleted}); err != nil || !moved {
		t.Fatal(moved, err)
	}
	h, err := host.New(t.Context(), host.Config{Directory: dir, Build: func(ctx context.Context, _ session.ID) (host.Runtime, error) {
		return host.Runtime{Builder: contextbuilder.NewBuilder(), LLM: archiveAnalysisModel{}, Tools: tool.NewRegistry(tool.StaticTranslators{}), Operations: operation.NewLocalOperationManager(ctx)}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	_, items, _, err := store.ReadSnapshot(t.Context(), "main")
	if err != nil {
		t.Fatal(err)
	}
	owner, err := h.Resume(t.Context(), host.Options{ID: "main", Configuration: items[0].Data.(sessionstore.HostRecord).Configuration})
	if err != nil {
		t.Fatal(err)
	}
	v, err := owner.InspectBefore(0, 1)
	if err != nil || !v.History.Older {
		t.Fatal("expected limited tail", err, v.History)
	}
	m := NewModel("main")
	if err := m.seedTail(v); err != nil {
		t.Fatal(err)
	}
	reader := viewer.ParentReader{Host: h, ParentID: "main", Directory: dir}
	handler := viewer.NewExtensionHandler(h, dir)
	remote := viewer.Remote{ParentID: "main", Parent: reader, Call: func(ctx context.Context, raw jsontext.Value) (jsontext.Value, error) {
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, "http://localhost/v1/extension", bytes.NewReader(raw))
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, req)
		return jsontext.Value(out.Body.Bytes()), nil
	}}
	panel := viewer.NewPanel(t.Context(), "main", viewer.NewClient(remote, viewer.New(viewer.SubagentOptions()), nil))
	defer panel.Close()
	u := UIState{View: ViewChat, Now: opusReset, Theme: Theme{Plain: true}}
	waitFor(t, func() bool {
		u.Viewer = panel.Snapshot(u.Now)
		return len(u.Viewer.SubscriptionUsage) == 3
	})
	bar := statisticsBar(m.Snapshot(), u, makeLayout(160, 40)).plain()
	for _, want := range []string{"ChatGPT 週 残り26%", "5h 残り100%", "Claude 5h 残り29%", "週 残り69%"} {
		if !strings.Contains(bar, want) {
			t.Fatalf("footer missing %q: %s", want, bar)
		}
	}
	if strings.Contains(bar, "Opus") || strings.Contains(bar, "超過") {
		t.Fatal("footer included expired or unknown usage", bar)
	}
	if err := m.EnsureAnalysis(t.Context(), reader); err != nil {
		t.Fatal(err)
	}
	report, err := m.readAnalysisTree(t.Context(), remote.InspectTree, u.Now, u.Viewer.Rows, nil)
	if err != nil {
		t.Fatal(err)
	}
	if report.Tree == nil || report.Tree.Partial || len(report.SubscriptionUsage) != 7 {
		t.Fatal("incomplete subscription analysis", report.SubscriptionUsage)
	}
	encoded, err := analysis.Encode(report, "json")
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct{ SubscriptionUsage []analysis.SubscriptionWindow }
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	find := func(provider, window string) analysis.SubscriptionWindow {
		t.Helper()
		i := slices.IndexFunc(decoded.SubscriptionUsage, func(w analysis.SubscriptionWindow) bool { return w.Provider == provider && w.Window == window })
		if i < 0 {
			t.Fatalf("JSON missing %s %s", provider, window)
		}
		return decoded.SubscriptionUsage[i]
	}
	for _, want := range []struct {
		provider, window, status string
		used                     float64
		observed, reset          time.Time
	}{
		{"openai-codex", "primary", "", 74, codexAt, weekReset},
		{"openai-codex", "secondary", "", 0, zeroAt, shortReset},
		{"claude", "five_hour", "allowed", 71, shortAt, shortReset},
		{"claude", "seven_day", "allowed_warning", 31, weekAt, weekReset},
	} {
		w := find(want.provider, want.window)
		if w.State != "observed" || w.UsedPercent == nil || *w.UsedPercent != want.used || w.Status != want.status || !w.ObservedAt.Equal(want.observed) || !w.ResetsAt.Equal(want.reset) {
			t.Fatalf("wrong observation for %s %s: %+v", want.provider, want.window, w)
		}
	}
	if w := find("claude", "seven_day_opus"); w.State != "reset_since" || w.UsedPercent != nil || w.Status != "rejected" || !w.ResetsAt.Equal(opusReset) || !w.ObservedAt.Equal(opusAt) {
		t.Fatalf("expired observation displayed as current: %+v", w)
	}
	if w := find("claude", "seven_day_sonnet"); w.State != "unknown" || w.UsedPercent != nil || !w.ObservedAt.IsZero() {
		t.Fatalf("unreported window acquired a value: %+v", w)
	}
	if w := find("claude", "overage"); w.State != "unknown" || w.UsedPercent != nil || w.Status != "allowed" || w.ObservedAt.IsZero() {
		t.Fatalf("status-only observation lost: %+v", w)
	}
	markdown, err := analysis.Encode(report, "markdown")
	if err != nil {
		t.Fatal(err)
	}
	_, section, _ := strings.Cut(string(markdown), "## Subscription usage\n")
	for _, want := range []string{"secondary / 100% left / used 0%", "reset since " + opusReset.Format(time.RFC3339), "observed " + opusAt.Format(time.RFC3339), "seven\\_day\\_sonnet / unknown"} {
		if !strings.Contains(section, want) {
			t.Fatalf("Markdown missing %q: %s", want, section)
		}
	}
	if strings.Contains(section, "89%") {
		t.Fatal("Markdown included an expired percent", section)
	}
	appendUsage(f.Lead, "claude", llm.SubscriptionUsage{Window: "seven_day", Status: "allowed_warning", UsedPercent: new(32.0), ResetsAt: weekReset})
	waitFor(t, func() bool {
		u.Viewer = panel.Snapshot(u.Now)
		return strings.Contains(statisticsBar(m.Snapshot(), u, makeLayout(160, 40)).plain(), "週 残り68%")
	})
	report = m.Analysis(u.Now, u.Viewer.Rows, u.Viewer.SubscriptionUsage)
	if lines := strings.Join(analysis.Lines(report, analysis.SubscriptionUsageView, true), "\n"); !strings.Contains(lines, "Claude seven_day / 68% left / used 32%") {
		t.Fatal("open analysis missed the live weekly update", lines)
	}

	awaitWeekly := func(label string) {
		t.Helper()
		waitFor(t, func() bool {
			u.Viewer = panel.Snapshot(u.Now)
			return strings.Contains(statisticsBar(m.Snapshot(), u, makeLayout(160, 40)).plain(), "週 "+label)
		})
	}
	checkStaleWeekly := func(status string) {
		t.Helper()
		awaitWeekly("—")
		report, err := m.readAnalysisTree(t.Context(), remote.InspectTree, u.Now, u.Viewer.Rows, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range report.SubscriptionUsage {
			if w.Provider == "claude" && w.Window == "seven_day" {
				if w.State != "stale" || w.UsedPercent != nil || w.Status != status {
					t.Fatalf("stale allowance displayed as current: %+v", w)
				}
				return
			}
		}
		t.Fatal("missing stale Claude weekly window")
	}
	rejection := llm.SubscriptionUsage{Window: "seven_day", Status: "rejected", UsedPercent: new(99.0), ResetsAt: weekReset}
	appendUsage(f.Lead, "claude", rejection)
	awaitWeekly("残り1%")
	appendUsage(f.Lead, "claude", llm.SubscriptionUsage{Window: rejection.Window, Status: "allowed", ResetsAt: weekReset})
	checkStaleWeekly("allowed")

	appendUsage(f.Lead, "claude", rejection)
	awaitWeekly("残り1%")
	child := f.Workers["flash-first"]
	turn := session.Turn{ID: "claude-recovery", PreviousTurnID: "turn", Type: session.TurnRegular}
	if err := store.AppendTurn(t.Context(), child, turn); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendModelResponse(t.Context(), child, sessionstore.ModelResponse{
		TurnID: turn.ID, Response: llm.Response{Provider: "claude", Stop: llm.StopComplete},
	}); err != nil {
		t.Fatal(err)
	}
	checkStaleWeekly("")
	appendUsage(f.Lead, "claude", llm.SubscriptionUsage{Window: rejection.Window, Status: "allowed", UsedPercent: new(2.0), ResetsAt: weekReset})
	awaitWeekly("残り98%")
}

func TestSubscriptionUsesLastPercentageFromSameCycle(t *testing.T) {
	now := time.Date(2026, 11, 23, 4, 0, 0, 0, time.UTC)
	reset := now.Add(24 * time.Hour)
	var latest llm.SubscriptionObservations
	latest.Observe(llm.ActivityRecord{Kind: llm.ActivitySubscriptionUsage, Provider: "claude", Subscription: []llm.SubscriptionUsage{
		{Window: "seven_day", Status: "rejected", ResetsAt: reset.Add(time.Second)},
	}}, now)
	// A different descendant can supply the earlier percentage after the
	// newer status has already been merged.
	var other llm.SubscriptionObservations
	other.Observe(llm.ActivityRecord{Kind: llm.ActivitySubscriptionUsage, Provider: "claude", Subscription: []llm.SubscriptionUsage{
		{Window: "seven_day", Status: "allowed_warning", UsedPercent: new(99.0), ResetsAt: reset},
	}}, now.Add(-time.Minute))
	latest.Merge(other)
	latest.Observe(llm.ActivityRecord{Kind: llm.ActivitySubscriptionUsage, Provider: "openai-codex", Subscription: []llm.SubscriptionUsage{
		{Window: "secondary", UsedPercent: new(0.0)},
	}}, now)
	windows := analysis.SubscriptionWindows(latest, now)
	var weekly, secondary analysis.SubscriptionWindow
	for _, w := range windows {
		switch w.Window {
		case "seven_day":
			weekly = w
		case "secondary":
			secondary = w
		}
	}
	if weekly.UsedPercent == nil || *weekly.UsedPercent != 99 || weekly.Status != "rejected" ||
		!weekly.ObservedAt.Equal(now) || !weekly.PercentTime().Equal(now.Add(-time.Minute)) {
		t.Fatalf("status and percentage came from different cycles or lost their timestamps: %+v", weekly)
	}
	if secondary.UsedPercent != nil || secondary.State != "unknown" {
		t.Fatalf("legacy placeholder became a full window: %+v", secondary)
	}
	r := analysis.Report{At: now, SubscriptionUsage: []analysis.SubscriptionWindow{weekly, secondary}}
	output := analysisText(r, analysis.SubscriptionUsageView, 99, false)
	_, secondaryRow, _ := strings.Cut(output, "ChatGPT secondary")
	if !strings.Contains(output, "used 99.0%  rejected") || !strings.Contains(secondaryRow, "—") ||
		strings.Contains(secondaryRow, "100.0%") || strings.Contains(secondaryRow, "steady") {
		t.Fatal("subscription status or placeholder was rendered incorrectly", output)
	}
	nextCycle := reset.AddDate(0, 0, 7)
	latest.Observe(llm.ActivityRecord{Kind: llm.ActivitySubscriptionUsage, Provider: "claude", Subscription: []llm.SubscriptionUsage{
		{Window: "seven_day", Status: "allowed", ResetsAt: nextCycle},
	}}, reset.Add(time.Minute))
	for _, w := range latest {
		if w.Window == "seven_day" && (w.UsedPercent != nil || !w.ResetsAt.Equal(nextCycle)) {
			t.Fatalf("percentage crossed a reset: %+v", w)
		}
	}
}

func TestSubscriptionResponseRecoveryUsesRecordedRuntime(t *testing.T) {
	now := time.Date(2026, 11, 23, 4, 0, 0, 0, time.UTC)
	m := NewModel("main")
	v := host.View{}
	appendItem := func(kind sessionstore.ItemKind, data any) {
		t.Helper()
		page := &v.History
		page.NextAfter++
		at := now.Add(time.Duration(page.NextAfter) * time.Second)
		page.Items = append(page.Items, host.HistoryItem{Sequence: page.NextAfter, RecordedAt: at, Kind: kind, Data: data})
		if err := m.apply(v); err != nil {
			t.Fatal(err)
		}
	}
	appendItem(sessionstore.ItemHostRecord, sessionstore.HostRecord{Kind: "configuration",
		Configuration: []byte(`{"Provider":{"provider":"claude","model":{"id":"claude-test"}}}`)})
	appendItem(sessionstore.ItemHostRecord, sessionstore.HostRecord{Kind: sessionstore.HostActivity, Activity: &llm.ActivityRecord{
		Kind: llm.ActivitySubscriptionUsage, Provider: "claude", Subscription: []llm.SubscriptionUsage{
			{Window: "seven_day", Status: "rejected", UsedPercent: new(99.0), ResetsAt: now.Add(time.Hour)},
			{Window: "five_hour", Status: "allowed_warning", UsedPercent: new(90.0), ResetsAt: now.Add(time.Hour)},
		},
	}})
	appendItem(sessionstore.ItemModelResponse, sessionstore.ModelResponse{
		Response: llm.Response{Provider: "openai-codex", Stop: llm.StopComplete},
	})
	appendItem(sessionstore.ItemModelResponse, sessionstore.ModelResponse{
		AdapterError: true, Response: llm.Response{Stop: llm.StopComplete},
	})
	u := UIState{Now: now.Add(time.Minute), Theme: Theme{Plain: true}}
	footer := func() string { return statisticsBar(m.Snapshot(), u, makeLayout(160, 40)).plain() }
	if got := footer(); !strings.Contains(got, "週 残り1%") {
		t.Fatal("an unrelated response cleared the percentage", got)
	}
	appendItem(sessionstore.ItemModelResponse, sessionstore.ModelResponse{Response: llm.Response{Stop: llm.StopComplete}})
	if got := footer(); !strings.Contains(got, "週 —") || !strings.Contains(got, "5h 残り10%") {
		t.Fatal("a successful response did not retire just the rejected percentage", got)
	}
}

func TestSubscriptionFooterWidthAndReset(t *testing.T) {
	now := time.Date(2026, 11, 21, 20, 50, 0, 0, time.Local)
	s := statsFixture("openai-codex")
	s.Version = 1
	s.SubscriptionUsage.Observe(llm.ActivityRecord{Kind: llm.ActivitySubscriptionUsage, Provider: "openai-codex", Subscription: []llm.SubscriptionUsage{
		{Window: "primary", UsedPercent: new(74.0), WindowMinutes: new(int64(10080)), ResetsAt: now.AddDate(0, 0, 3)},
	}}, now)
	u := UIState{Now: now, Theme: Theme{Plain: true, Still: true}, Viewer: viewer.PanelSnapshot{Version: 1}, cache: &bodyCache{}}
	reset := now.Add(4 * time.Hour)
	u.Viewer.SubscriptionUsage.Observe(llm.ActivityRecord{Kind: llm.ActivitySubscriptionUsage, Provider: "claude", Subscription: []llm.SubscriptionUsage{
		{Window: "five_hour", UsedPercent: new(71.0), ResetsAt: reset},
		{Window: "seven_day", UsedPercent: new(31.0)},
	}}, now)
	for _, width := range []int{19, 44, 80, 120, 160} {
		f := RenderFrame(s, u, width, 24)
		bounds(t, f, width, 24)
		bar := f.Lines[len(f.Lines)-1]
		if width >= 80 && (!strings.Contains(bar, "ChatGPT 週 残り26%") || !strings.Contains(bar, "Claude 5h 残り29%")) {
			t.Fatal("room for both providers", width, bar)
		}
		if width == 44 && (!strings.Contains(bar, "10.0 tok/s") || strings.Contains(bar, "ChatGPT") || strings.Contains(bar, "Claude")) {
			t.Fatal("narrow footer did not drop complete subscription windows", bar)
		}
		if width == 160 && (!strings.Contains(bar, "(11/24 20:50)") || !strings.Contains(bar, "(00:50)") || !strings.Contains(bar, "週 残り69%")) {
			t.Fatal("missing reset times or weekly observation", bar)
		}
	}
	if deadline := nextRender(s, u, ViewChat); !deadline.Equal(reset) {
		t.Fatal("idle footer does not schedule reset", deadline)
	}
	u.Now = reset
	f := RenderFrame(s, u, 160, 24)
	if bar := f.Lines[len(f.Lines)-1]; strings.Contains(bar, "残り29%") || !strings.Contains(bar, "Claude 週 残り69%") || !strings.Contains(bar, "ChatGPT 週 残り26%") {
		t.Fatal("cached footer kept a window at its reset", bar)
	}
}

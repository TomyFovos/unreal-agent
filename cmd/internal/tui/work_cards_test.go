package tui

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
)

func plainRows(rows []line) string {
	var text []string
	for _, row := range rows {
		text = append(text, row.plain())
	}
	return strings.Join(text, "\n")
}

func TestWorkCardsTitleOperationAndInternalMetadata(t *testing.T) {
	s, u := orchestrationFixture()
	r := &u.Viewer.Rows[1]
	r.ID = "0123456789abcdef0123456789abcdef"
	r.Label = "日本語の題名はIDより優先\nこの行は出さない"
	r.Operations = []viewer.OperationRow{
		{ID: "newest-completed", Tool: "Write", Target: "completed.go", Status: operation.StatusCompleted, Sequence: 100},
		{ID: "older-active", Tool: "Read", Target: "old.go", Status: operation.StatusReady, Sequence: 20},
		{ID: "latest-active", Tool: "claude_code", Target: "Bash go test ./harness/...", Status: operation.StatusAwaiting, Sequence: 30},
	}
	o := BuildOrchestration(s, u.Viewer, u.Now, 128)
	rows, _, _ := workCardPane(o, u, 90, 30, false)
	text := plainRows(rows)
	for _, want := range []string{"◆ メイン", "[A] 日本語の題名はIDより優先", "Bash go test ./harness/...", "gpt-fixture·medium"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q:\n%s", want, text)
		}
	}
	for _, forbidden := range []string{"0123456789abcdef0123456789abcdef", "child-a", "child-b", "child-c", "main", "r0", "turn:", "observed:", "Host validates", "owns requests", "active path", "latest-active", "older-active", "claude_code", "completed.go", "old.go", "この行は出さない", "queued"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("internal/obsolete text %q:\n%s", forbidden, text)
		}
	}
	for _, tc := range []struct{ activity, want string }{{"tool: Read AGENTS.md", "Read AGENTS.md"}, {"input: 質問", "受信: 質問"}, {"", ""}} {
		r.Operations, r.Activity = nil, tc.activity
		if got := currentChildOperation(*r); got != tc.want {
			t.Fatalf("fallback %q = %q", tc.activity, got)
		}
	}
	r.Generation, r.Cursor, r.Activity = "", 0, "unobserved activity"
	if got := currentChildOperation(*r); got != "" {
		t.Fatalf("unobserved operation = %q", got)
	}
}

func TestWorkCardsStallAtFiveMinutesChangesStripeAndText(t *testing.T) {
	s, u := orchestrationFixture()
	u.Now = time.Unix(10000, 0)
	u.Viewer.Rows[1].LastActivityAt = u.Now.Add(-5 * time.Minute)
	for _, tc := range []struct {
		now  time.Time
		role colorRole
		text string
	}{{u.Now.Add(-time.Second), stateRun, "Bash go test"}, {u.Now, stateStall, "動きなし 5分"}, {u.Now.Add(3 * time.Minute), stateStall, "動きなし 8分"}} {
		o := BuildOrchestration(s, u.Viewer, tc.now, 128)
		card := o.cards[1]
		rows := workCardRows(card, 70, Theme{Depth: ColorTrue}, false)
		if rows[0][0].style.fg != tc.role || rows[1][0].style.fg != tc.role || !strings.Contains(rows[1].plain(), tc.text) {
			t.Fatalf("stripe/text mismatch at %s: %s, %+v", tc.now, plainRows(rows), rows[0][0].style)
		}
		if !strings.Contains(rows[0].paint(Theme{Depth: ColorTrue}), Theme{Depth: ColorTrue}.colorSGR(tc.role, false)) {
			t.Fatal("stripe omitted state color")
		}
	}
	u.Viewer.Rows[1].LastActivityAt = time.Time{}
	if BuildOrchestration(s, u.Viewer, u.Now, 128).cards[1].stalled {
		t.Fatal("unknown activity time guessed as stalled")
	}
}

func TestWorkCardsRetainFailureAfterUserOrWithActiveDescendants(t *testing.T) {
	now := time.Unix(10000, 0)
	for _, tc := range []struct {
		name             string
		finish           time.Time
		activeDescendant bool
		keep             bool
	}{{"before user", now.Add(-2 * time.Minute), false, false}, {"same as user", now.Add(-time.Minute), false, false}, {"after user", now.Add(-30 * time.Second), false, true}, {"active descendant", now.Add(-2 * time.Minute), true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			s, u := idleSnapshot(), plainUI()
			s.Entries = []Entry{{Role: "you", At: now.Add(-time.Minute)}}
			u.Viewer.Rows = []viewer.Row{{ID: s.ID}, {ID: "failed-id", ParentID: s.ID, Label: "失敗した調査", ParentOperationStatus: operation.StatusFailed, Finish: &viewer.Finish{Status: "failure", RecordedAt: tc.finish}}}
			if tc.activeDescendant {
				u.Viewer.Rows = append(u.Viewer.Rows, viewer.Row{ID: "grandchild", ParentID: "failed-id", Label: "継続する作業", ParentOperationStatus: operation.StatusAwaiting, Depth: 2})
			}
			o := BuildOrchestration(s, u.Viewer, now, 128)
			text := plainRows(cardTestRows(o, 80, Theme{Plain: true}))
			if strings.Contains(text, "失敗した調査") != tc.keep {
				t.Fatal("failure retention", text)
			}
			if tc.keep && (!strings.Contains(text, "✗ 失敗") || len(o.beads) != 0 || cardColorStyle(o.cards[1]).fg != stateBad) {
				t.Fatal("retained failure is not a red card", text)
			}
			if tc.keep && strings.Count(text, "✗ 失敗") != 1 {
				t.Fatal("empty current operation duplicated the failure label", text)
			}
			if !tc.keep && (len(o.beads) != 1 || !strings.Contains(text, "終了 ✗")) {
				t.Fatal("old failure was not a bead", text)
			}
			if tc.activeDescendant && !strings.Contains(text, "  ▌[B] 継続する作業") {
				t.Fatal("failed ancestor or grandchild indentation lost", text)
			}
		})
	}
}

func TestWorkCardsLineageCreationOrderAndDeterministicMarks(t *testing.T) {
	s, u := idleSnapshot(), plainUI()
	now := u.Now
	u.Viewer.Rows = []viewer.Row{
		{ID: s.ID},
		{ID: "b", ParentID: s.ID, Label: "第二の子", CreatedAt: now.Add(-3 * time.Minute), ParentOperationStatus: operation.StatusAwaiting},
		{ID: "grand", ParentID: "a", Label: "第一の孫", CreatedAt: now.Add(-2 * time.Minute), ParentOperationStatus: operation.StatusReady},
		{ID: "a", ParentID: s.ID, Label: "第一の子", CreatedAt: now.Add(-4 * time.Minute), ParentOperationStatus: operation.StatusReady},
	}
	first := BuildOrchestration(s, u.Viewer, now, 64)
	want := []session.ID{s.ID, "a", "grand", "b"}
	var got []session.ID
	for _, c := range first.cards {
		got = append(got, c.id)
	}
	if !slices.Equal(got, want) || first.cards[1].mark != "A" || first.cards[2].mark != "C" || first.cards[3].mark != "B" || first.cards[2].depth != 1 {
		t.Fatal("lineage/order/marks", first.cards)
	}
	slices.Reverse(u.Viewer.Rows)
	second := BuildOrchestration(s, u.Viewer, now, 64)
	if !reflect.DeepEqual(first.cards, second.cards) {
		t.Fatal("reattach reordered cards/marks")
	}
}

func TestWorkCardsActivityLevelsCreationAndObservation(t *testing.T) {
	now := time.Unix(10000, 0)
	r := viewer.Row{Generation: "g", CreatedAt: now.Add(-10 * time.Minute)}
	counts := []int{0, 1, 2, 3, 4, 5, 7, 8, 12, 0}
	for i, n := range counts {
		for range n {
			r.RecentActivity = append(r.RecentActivity, now.Add(-10*time.Minute+time.Duration(i)*time.Minute+30*time.Second))
		}
	}
	r.RecentActivity = append(r.RecentActivity, now.Add(-11*time.Minute), now.Add(time.Second))
	c := workCard{observed: observed(r), activity: activityBuckets(r, now)}
	if got := activityBars(c, Theme{}).plain(); got != "─▁▂▃▃▅▅▇▇─" {
		t.Fatalf("fixed levels = %q", got)
	}
	if got := activityBars(c, Theme{ASCII: true}).plain(); got != "-.:--==##-" {
		t.Fatalf("ASCII levels = %q", got)
	}
	if st := activityBars(c, Theme{})[0].style; st.fg != dimColor {
		t.Fatal("zero activity is not dim")
	}
	r.CreatedAt = now.Add(-150 * time.Second)
	c.activity = activityBuckets(r, now)
	if got := activityBars(c, Theme{}).plain(); got != "       ▇▇─" {
		t.Fatalf("pre-creation buckets = %q", got)
	}
	r.CreatedAt, r.RecentActivity = now, []time.Time{now}
	c.activity = activityBuckets(r, now)
	if got := activityBars(c, Theme{}).plain(); got != "         ▁" {
		t.Fatalf("activity exactly at creation/now = %q", got)
	}
	c.observed = false
	if len(activityBars(c, Theme{})) != 0 {
		t.Fatal("unobserved activity drawn")
	}
}

func TestWorkCardsBeadsNewestAtRightAndExactOverflow(t *testing.T) {
	var beads []workBead
	for i := range 800 {
		beads = append(beads, workBead{state: stateCompleted, at: time.Unix(int64(i), 0)})
	}
	beads[798].state, beads[799].state = stateFailed, stateCanceled
	row := finishedBeads(beads, 30, Theme{})
	if row.width() != 30 || row.plain() != "終了 ✓✓✓✓✓✓✓✓✓✓✓✓✓✗⊘  ほか 785" {
		t.Fatalf("beads = %q (%d)", row.plain(), row.width())
	}
	if row[3+13].style.fg != stateBad || row[3+14].style.fg != dimColor {
		t.Fatal("failure/cancel bead colors", row)
	}
	if got := finishedBeads(beads[797:], 30, Theme{}).plain(); got != "終了 ✓✗⊘" {
		t.Fatalf("all beads = %q", got)
	}
}

func TestWorkCardsTwoRowsSpacingModelAndOverflow(t *testing.T) {
	s, u := orchestrationFixture()
	o := BuildOrchestration(s, u.Viewer, u.Now, 128)
	rows, _, _ := workCardPane(o, u, 70, 12, false)
	if len(rows) != 6 || !strings.Contains(rows[3].plain(), "gpt-fixture·medium") || len(rows[2]) != 0 {
		t.Fatal("tall pane lacks two-line cards/spacing", plainRows(rows))
	}
	rows, _, _ = workCardPane(o, u, 70, 5, true)
	if len(rows) != 5 || rows[3][1].style.bg != cardAltColor || !strings.Contains(rows[2].plain(), "gpt-fixture·medium") {
		t.Fatal("compact pane lacks alternating two-line cards", plainRows(rows))
	}
	for i := range 10 {
		u.Viewer.Rows = append(u.Viewer.Rows, viewer.Row{ID: session.ID(fmt.Sprintf("overflow-%02d", i)), ParentID: s.ID, Label: fmt.Sprintf("overflow task %02d", i), ParentOperationStatus: operation.StatusAwaiting})
	}
	o = BuildOrchestration(s, u.Viewer, u.Now, 128)
	rows, _, _ = workCardPane(o, u, 40, 8, true)
	if len(rows) != 8 || rows[7].plain() != "＋9 件  /view orchestration" || !strings.Contains(rows[0].plain(), "◆ メイン") || !strings.HasPrefix(rows[6].plain(), "終了 ") {
		t.Fatal("overflow budget/count", plainRows(rows))
	}
}

func TestWorkCardsModelEffortBeforeElapsedAndTitleTruncation(t *testing.T) {
	c := workCard{mark: "A", title: "長い作業の題名を先に省略するための表示テスト", model: "gpt-6.1-sol · xhigh", elapsed: "12m", state: stateWorking}
	for _, width := range []int{44, 100} {
		rows := workCardRows(c, width, Theme{Plain: true}, false)
		if len(rows) != 2 || rows[0].width() != width || !strings.HasSuffix(rows[0].plain(), " gpt-6.1-sol·xhigh 12m") || strings.Contains(rows[1].plain(), c.model) {
			t.Fatal("model/effort/elapsed must be on the first of two rows", plainRows(rows))
		}
		if strings.Contains(rows[0].plain(), c.title) != (width == 100) {
			t.Fatal("title must be truncated before the model/effort", plainRows(rows))
		}
	}
	if first := workCardRows(c, 100, Theme{Plain: true, ASCII: true}, false)[0].plain(); !strings.HasSuffix(first, " gpt-6.1-sol/xhigh 12m") {
		t.Fatal("ASCII model/effort separator", first)
	}
	c.state = stateFailed
	if first := workCardRows(c, 100, Theme{Plain: true}, false)[0].plain(); !strings.HasSuffix(first, " gpt-6.1-sol·xhigh ✗ 失敗") {
		t.Fatal("failed child's model/effort lost", first)
	}
	s, u := orchestrationFixture()
	u.Viewer.Rows[1].Elapsed = viewer.Duration{Known: true, Value: 12 * time.Minute}
	for _, view := range []ViewMode{ViewSplit, ViewOrchestration} {
		u.View = view
		frame := RenderFrame(s, u, 160, 40)
		found := false
		for _, row := range frame.Lines {
			if strings.Contains(row, "[A]") && strings.Contains(row, "gpt-fixture·medium 12m") {
				found = true
			}
		}
		if !found {
			t.Fatalf("view %s lacks the child's model before elapsed: %s", view, frameText(frame))
		}
	}
}

func TestWorkCardsBeadOrderDoesNotGuessMissingFinishTime(t *testing.T) {
	s, u := idleSnapshot(), plainUI()
	u.Viewer.Rows = []viewer.Row{
		{ID: "b", ParentID: s.ID, ParentOperationStatus: operation.StatusCanceled, LastActivityAt: u.Now.Add(time.Hour)},
		{ID: "z", ParentID: s.ID, ParentOperationStatus: operation.StatusCompleted, Finish: &viewer.Finish{Status: "success", RecordedAt: u.Now.Add(-time.Minute)}},
		{ID: "a", ParentID: s.ID, ParentOperationStatus: operation.StatusCompleted, LastActivityAt: u.Now.Add(2 * time.Hour)},
	}
	o := BuildOrchestration(s, u.Viewer, u.Now, 64)
	if len(o.beads) != 3 || o.beads[0].id != "a" || o.beads[1].id != "b" || o.beads[2].id != "z" || !o.beads[0].at.IsZero() || !o.beads[1].at.IsZero() {
		t.Fatal("missing Finish time was inferred from activity", o.beads)
	}
	if got := finishedBeads(o.beads, 30, Theme{}).plain(); got != "終了 ▪⊘✓" {
		t.Fatalf("bead order = %q", got)
	}
}

func TestWorkCardsMainPulseJapanese(t *testing.T) {
	now := time.Unix(10000, 0)
	for _, tc := range []struct {
		progress *host.Progress
		want     string
	}{{&host.Progress{Mode: "streaming"}, "考え中 6s"}, {&host.Progress{Mode: "streaming", Text: "draft"}, "生成中 6s"}, {&host.Progress{Attempt: 3}, "再試行中（3回目） 6s"}, {&host.Progress{Mode: "complete"}, "応答待ち 6s"}} {
		s := idleSnapshot()
		s.Progress, s.LatestAt = tc.progress, now.Add(-6*time.Second)
		if got := mainCardPulse(s, viewer.Row{}, nil, now); got != tc.want {
			t.Fatalf("pulse = %q, want %q", got, tc.want)
		}
	}
	s := idleSnapshot()
	s.Operations = []operation.Operation{{ID: "tool", ToolName: "claude_code", Status: operation.StatusAwaiting}}
	r := viewer.Row{Operations: []viewer.OperationRow{{ID: "tool", Tool: "claude_code", Target: "Bash go test", Elapsed: viewer.Duration{Known: true, Value: 4 * time.Second}}}}
	if got := mainCardPulse(s, r, nil, now); got != "Bash 実行中 4s" {
		t.Fatal("wrapper pulse", got)
	}
	s.Operations = append(s.Operations, operation.Operation{ID: "tool2", ToolName: "Read", Status: operation.StatusReady})
	if got := mainCardPulse(s, r, nil, now); got != "ツール2件 実行中 4s" {
		t.Fatal("multiple tools pulse", got)
	}
	s.Operations = nil
	if got := mainCardPulse(s, r, []viewer.Row{{ParentOperationStatus: operation.StatusAwaiting}}, now); got != "待機中（子 1件 作業中）" {
		t.Fatal("idle children pulse", got)
	}
}

func TestChildDockSingleLineBadgesStatesAndOverflow(t *testing.T) {
	s, u := idleSnapshot(), childUI(30)
	rows := childDock(s, u, makeLayout(45, 20), false)
	if len(rows) != 1 || rows[0].plain() != "子  [A]▸ [B]▸ [C]▸ +27   作業中 30 · 失敗 0" || rows[0].width() > 44 {
		t.Fatal("one-line dock", plainRows(rows), rows[0].width())
	}
	u = childUI(4)
	u.Viewer.Rows[1].ParentOperationStatus = operation.StatusAwaiting
	u.Viewer.Rows[2].ParentOperationStatus = operation.StatusCanceling
	u.Viewer.Rows[3].ParentOperationStatus = operation.StatusFailed
	u.Viewer.Rows[4].NeedsResync = true
	s.Permissions.Pending = []permission.Pending{{ID: "op-0"}}
	rows = childDock(s, u, makeLayout(100, 24), false)
	if got := rows[0].plain(); got != "子  [A]▸ [B]▸ [C]✗ [D]?   作業中 2 · 失敗 1  不明 1" {
		t.Fatalf("typed states = %q", got)
	}
	u.Now = time.Unix(10000, 0)
	u.Viewer.Rows[1].LastActivityAt = u.Now.Add(-5 * time.Minute)
	rows = childDock(s, u, makeLayout(100, 24), false)
	if !strings.Contains(rows[0].plain(), "[A]!") || rows[0][6].style.fg != stateStall {
		t.Fatal("stalled dock state", plainRows(rows))
	}
	u.Viewer.Rows[4].NeedsResync = false
	u.Viewer.Rows[4].ParentOperationStatus = operation.StatusCompleted
	u.Viewer.Rows[4].Finish = &viewer.Finish{Status: "success"}
	rows = childDock(s, u, makeLayout(100, 24), false)
	if got := rows[0].plain(); strings.Contains(got, "[D]") || !strings.Contains(got, "[C]✗") {
		t.Fatalf("finished child stays in the dock or failure left it: %q", got)
	}
}

func TestCardElapsedDropsSecondsPastAMinute(t *testing.T) {
	for d, want := range map[time.Duration]string{6 * time.Second: "6s", 12 * time.Minute: "12m", 59*time.Minute + 59*time.Second: "59m", 75 * time.Minute: "1h15m"} {
		if got := coarseDuration(d); got != want {
			t.Errorf("coarseDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestChildFocusJapaneseLabelsBadgeAndTitle(t *testing.T) {
	s, u := idleSnapshot(), childUI(2)
	r := u.Viewer.Rows[1]
	r.Generation, r.Activity = "observed", "tool: Read AGENTS.md"
	r.Selection = &sessionstore.RuntimeSelection{Model: "gpt-model", Effort: "high"}
	text := plainRows(focusRows(r, children(u, s), s, u, makeLayout(110, 30)))
	for _, want := range []string{"子", "[A] Task 0", "作業中", "活動", "Read AGENTS.md", "終了報告", "未記録", "ほか", "[B] Task 1"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing focus label %q: %s", want, text)
		}
	}
	for _, gone := range []string{"child-0000", "child-0001", "codex-work-1", "activity", "finish", "others", "queued"} {
		if strings.Contains(text, gone) {
			t.Fatalf("legacy focus label %q: %s", gone, text)
		}
	}
}

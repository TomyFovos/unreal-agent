package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/opencodego"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/subagent"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
)

// Use the production plan decoder, including main's worker-max template
// resolved to the lead layer. File observations deliberately have no generation:
// RuntimeUnknown describes ownership, not whether the lead is doing work.
func leadActivityFixture(t *testing.T, live, workers bool) (Snapshot, UIState, *viewer.Model, []session.ID, []operation.Operation) {
	t.Helper()
	s, u := idleSnapshot(), plainUI()
	s.ID, s.Generation, s.Version = "main", "main-generation", 1
	u.Now = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	u.Theme = Theme{Depth: ColorTrue}
	u.cache = &bodyCache{}
	m := viewer.New(viewer.SubagentOptions())
	dir := t.TempDir()
	start := func(parent session.ID, id operation.ID, layer, provider, model string) operation.Operation {
		t.Helper()
		spec, err := subagent.NewSpec(subagent.Plan{Version: 1, Action: "start", ParentID: parent, Template: "worker-max", Text: string(id), Configuration: &subagent.Template{
			Layer: layer, Workspace: dir, Runtime: []byte(fmt.Sprintf(`{"Provider":{"provider":%q,"model":{"id":%q}},"ReasoningEffort":"max"}`, provider, model)),
		}})
		if err != nil {
			t.Fatal(err)
		}
		return operation.Operation{ID: id, Type: spec.Type, Version: spec.Version, State: spec.State, MaxOutputLength: spec.MaxOutputLength, ToolName: "SubagentStart", Status: operation.StatusAwaiting}
	}
	var leads []session.ID
	var workerOps []operation.Operation
	main := host.View{Session: sessionstore.Snapshot{Session: session.Session{ID: s.ID}}, Generation: s.Generation, Revision: 1, Running: true}
	for i := range 4 {
		op := start(s.ID, operation.ID(fmt.Sprintf("lead-%d", i)), "lead", "openai-codex", "gpt-6.2-sol")
		main.Operations = append(main.Operations, op)
		lead := subagent.ChildID(s.ID, op.ID)
		leads = append(leads, lead)
		workerOp := start(lead, "worker", "worker", opencodego.ProviderID, "deepseek-v4.1-flash")
		workerOps = append(workerOps, workerOp)
		item := host.HistoryItem{Sequence: 1, RecordedAt: u.Now.Add(-7 * time.Minute), Kind: sessionstore.ItemModelResponse, Data: sessionstore.ModelResponse{TurnID: "lead-turn", Response: llm.Response{Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "結果を確認する。" + strings.Repeat("作業経過の説明。", 8)}}}}}}
		v := host.View{Session: sessionstore.Snapshot{Session: session.Session{ID: lead, CreatedAt: u.Now.Add(-15 * time.Minute)}}, History: host.HistoryPage{Items: []host.HistoryItem{item}, NextAfter: 1}}
		if live {
			v.Generation, v.Revision, v.Running = "lead-generation", 1, true
			v.Progress = &host.Progress{TurnID: "lead-turn", Mode: "streaming", Text: "作業を続ける"}
		}
		if workers {
			v.Operations = []operation.Operation{workerOp}
		}
		if err := m.Replace(lead, v); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Replace(s.ID, main); err != nil {
		t.Fatal(err)
	}
	u.Viewer = viewer.PanelSnapshot{ParentID: s.ID, Selected: s.ID}
	u.Viewer.Rows, u.Viewer.Version = m.RowsVersion(u.Now)
	for _, r := range u.Viewer.Rows {
		if r.ID != s.ID && r.ParentID == "" {
			t.Fatalf("fixture lost canonical child lineage: %+v", u.Viewer.Rows)
		}
	}
	return s, u, m, leads, workerOps
}

func leadCard(t *testing.T, cards []workCard, id session.ID) workCard {
	t.Helper()
	for _, card := range cards {
		if card.id == id {
			return card
		}
	}
	t.Fatalf("missing lead card %s in %+v", id, cards)
	return workCard{}
}

func TestRunningLeadWithWorkerKeepsWorkingPresentation(t *testing.T) {
	for _, live := range []bool{false, true} {
		t.Run(fmt.Sprintf("live-mid-turn=%v", live), func(t *testing.T) {
			s, u, m, leads, _ := leadActivityFixture(t, live, true)
			for _, mode := range []ViewMode{ViewSplit, ViewOrchestration} {
				u.View = mode
				frame := cachedFrameOracle(t, s, u, 180, 40)
				if strings.Count(strings.Join(frame.Lines, "\n"), "子が作業中") < len(leads) {
					t.Errorf("%s clipped the worker activity labels from the lead rows", mode)
				}
				for _, id := range leads {
					card := leadCard(t, u.cache.cards, id)
					if card.state != stateWorking || card.stalled || cardColorStyle(card).fg != stateRun || !strings.Contains(card.now, "子が作業中") {
						t.Errorf("%s lead incorrectly warns while worker runs: %+v", mode, card)
					}
					rows := workCardRows(card, 80, u.Theme, false)
					if rows[0][0].style.fg != stateRun || rows[1][0].style.fg != stateRun || strings.Contains(plainRows(rows), "動きなし") {
						t.Errorf("%s lead stripe/label: %s", mode, plainRows(rows))
					}
				}
				// Advance only the clock: cached rows must retain descendant work.
				u.Now = u.Now.Add(6 * time.Minute)
				u.Viewer.Rows, u.Viewer.Version = m.RowsVersion(u.Now)
				cachedFrameOracle(t, s, u, 180, 40)
				for _, id := range leads {
					if card := leadCard(t, u.cache.cards, id); card.stalled || cardColorStyle(card).fg != stateRun {
						t.Errorf("clock made working lead stall: %+v", card)
					}
				}
			}
			for _, row := range children(u, s) {
				if got := childVisualState(row, s); got != stateWorking || !row.UsageWaitUntil.IsZero() {
					t.Errorf("canonical running lineage has state %v: %+v", got, row)
				}
				if row.Depth == 2 {
					card := leadCard(t, u.cache.cards, row.ID)
					if card.stalled || cardColorStyle(card).fg != stateRun || row.Selection == nil || row.Selection.Provider != opencodego.ProviderID || row.Selection.Model != "deepseek-v4.1-flash" {
						t.Errorf("worker no longer shows its running state/runtime: %+v %+v", card, row)
					}
				}
				if row.Depth == 1 && (row.Activity == "" || row.LastActivityAt.IsZero() || row.Selection == nil || row.Selection.Model != "gpt-6.2-sol" || row.Selection.Effort != "max") {
					t.Errorf("lead projection lost its activity/runtime: %+v", row)
				}
			}
			u.View, u.index = ViewChat, nil
			for _, row := range childDock(s, u, makeLayout(100, 30), true) {
				for _, cell := range row {
					if cell.style.fg == stateStall {
						t.Errorf("dock warns about working lineage: %s", row.plain())
					}
				}
			}
		})
	}
}

func TestLeadLabelUpdatesOnWorkerEventsWithoutMainOrClockChanges(t *testing.T) {
	s, u, m, leads, workerOps := leadActivityFixture(t, true, false)
	u.View = ViewSplit
	refresh := func() {
		t.Helper()
		u.Viewer.Rows, u.Viewer.Version = m.RowsVersion(u.Now)
		cachedFrameOracle(t, s, u, 180, 40)
	}
	refresh()
	if card := leadCard(t, u.cache.cards, leads[0]); card.stalled || cardColorStyle(card).fg != stateRun || strings.Contains(card.now, "子が作業中") {
		t.Fatal("live lead's own turn must start working without a worker label", card)
	}
	before := u.Viewer.Version
	if err := m.Apply(leads[0], host.Event{Kind: "operation", Generation: "lead-generation", Revision: 2, Operation: &workerOps[0]}); err != nil {
		t.Fatal(err)
	}
	refresh()
	if card := leadCard(t, u.cache.cards, leads[0]); u.Viewer.Version <= before || card.stalled || cardColorStyle(card).fg != stateRun || !strings.Contains(card.now, "子が作業中") {
		t.Fatal("worker start did not update the cached lead", card)
	}
	workerOps[0].Status = operation.StatusCompleted
	item := host.HistoryItem{Sequence: 2, RecordedAt: u.Now, Kind: sessionstore.ItemToolCallStatus, Data: sessionstore.ToolCallStatus{Operations: []operation.Operation{workerOps[0]}}}
	if err := m.Apply(leads[0], host.Event{Kind: "item", Generation: "lead-generation", Revision: 3, Item: &item}); err != nil {
		t.Fatal(err)
	}
	refresh()
	if card := leadCard(t, u.cache.cards, leads[0]); card.stalled || cardColorStyle(card).fg != stateRun || strings.Contains(card.now, "子が作業中") {
		t.Fatal("worker end left the lead label stale", card)
	}
	// The lead continues its own turn after the worker finishes. Streaming
	// does not advance LastActivityAt, so a clock tick must not imply a stall.
	u.Now = u.Now.Add(6 * time.Minute)
	refresh()
	if card := leadCard(t, u.cache.cards, leads[0]); card.stalled || cardColorStyle(card).fg != stateRun || strings.Contains(card.now, "子が作業中") {
		t.Fatal("lead's own turn became stalled after its worker ended", card)
	}
}

func TestWorkingDescendantsPreserveLeadOutcomeAndWaitStates(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*viewer.Row, *Snapshot, time.Time)
		want   visualState
		color  colorRole
	}{
		{"ended", func(r *viewer.Row, _ *Snapshot, _ time.Time) { r.ParentOperationStatus = operation.StatusCompleted }, stateEnded, dimColor},
		{"canceled", func(r *viewer.Row, _ *Snapshot, _ time.Time) { r.ParentOperationStatus = operation.StatusCanceled }, stateCanceled, dimColor},
		{"canceling", func(r *viewer.Row, _ *Snapshot, _ time.Time) { r.ParentOperationStatus = operation.StatusCanceling }, stateCanceling, stateStall},
		{"failed", func(r *viewer.Row, _ *Snapshot, _ time.Time) { r.ParentOperationStatus = operation.StatusFailed }, stateFailed, stateBad},
		{"finish", func(r *viewer.Row, _ *Snapshot, now time.Time) {
			r.Finish = &viewer.Finish{Status: "completed", RecordedAt: now}
		}, stateCompleted, stateDone},
		{"end record", func(r *viewer.Row, _ *Snapshot, now time.Time) {
			r.End = &sessionstore.EndRecord{EndedBy: "host_restart", EndedAt: now}
		}, stateEnded, dimColor},
		{"usage wait", func(r *viewer.Row, _ *Snapshot, now time.Time) { r.UsageWaitUntil = now.Add(time.Hour) }, stateUsageWait, stateStall},
		{"unknown", func(r *viewer.Row, _ *Snapshot, _ time.Time) { r.NeedsResync = true }, stateUnknown, stateStall},
		{"disconnected", func(_ *viewer.Row, s *Snapshot, _ time.Time) { s.Connected = false }, stateUnknown, stateStall},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, u, _, leads, _ := leadActivityFixture(t, false, true)
			for i := range u.Viewer.Rows {
				if u.Viewer.Rows[i].ID == leads[0] {
					tc.change(&u.Viewer.Rows[i], &s, u.Now)
				}
			}
			cards, beads := buildWorkCards(s, u.Viewer, u.Now)
			for _, bead := range beads {
				if bead.id == leads[0] {
					if bead.state != tc.want {
						t.Fatalf("lead outcome changed: %+v", bead)
					}
					return
				}
			}
			card := leadCard(t, cards, leads[0])
			if card.state != tc.want || card.stalled || cardColorStyle(card).fg != tc.color || strings.Contains(card.now, "子が作業中") {
				t.Fatalf("descendants hid lead outcome/wait: %+v", card)
			}
		})
	}
}

func TestInactiveWorkerDoesNotHideLeadInactivity(t *testing.T) {
	s, u, _, leads, _ := leadActivityFixture(t, false, true)
	for _, tc := range []struct {
		name   string
		change func(*viewer.Row)
	}{
		{"ended", func(r *viewer.Row) { r.ParentOperationStatus = operation.StatusCompleted }},
		{"canceled", func(r *viewer.Row) { r.ParentOperationStatus = operation.StatusCanceled }},
		{"failed", func(r *viewer.Row) { r.ParentOperationStatus = operation.StatusFailed }},
		{"usage wait", func(r *viewer.Row) { r.UsageWaitUntil = u.Now.Add(time.Hour) }},
		{"unknown", func(r *viewer.Row) { r.NeedsResync = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			panel := u.Viewer
			panel.Rows = append([]viewer.Row(nil), panel.Rows...)
			for i := range panel.Rows {
				if panel.Rows[i].ParentID == leads[0] {
					tc.change(&panel.Rows[i])
				}
			}
			cards, _ := buildWorkCards(s, panel, u.Now)
			if card := leadCard(t, cards, leads[0]); !card.stalled || cardColorStyle(card).fg != stateStall || strings.Contains(card.now, "子が作業中") {
				t.Fatalf("inactive worker hid inactivity: %+v", card)
			}
		})
	}
}

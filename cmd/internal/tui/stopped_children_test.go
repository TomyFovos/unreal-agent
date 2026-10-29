package tui

import (
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
)

func TestStoppedParentChildrenUseSameStateInAllDisplays(t *testing.T) {
	for _, tc := range []struct {
		name    string
		runtime viewer.Liveness
		status  operation.Status
		finish  *viewer.Finish
		want    visualState
		working int
	}{
		{"live", viewer.RuntimeRunning, operation.StatusAwaiting, nil, stateWorking, 2},
		{"failed lead old record", viewer.RuntimeStopped, operation.StatusAwaiting, nil, stateEnded, 0},
		{"no finish after failure", viewer.RuntimeStopped, operation.StatusFailed, nil, stateEnded, 0},
		{"canceled grandchild", viewer.RuntimeStopped, operation.StatusCanceled, nil, stateCanceled, 0},
		{"canceling grandchild", viewer.RuntimeStopped, operation.StatusCanceling, nil, stateCanceled, 0},
		{"explicit finish", viewer.RuntimeStopped, operation.StatusAwaiting, &viewer.Finish{Status: "completed"}, stateCompleted, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, u := idleSnapshot(), plainUI()
			leadStatus := operation.StatusCanceled
			if tc.runtime == viewer.RuntimeRunning {
				leadStatus = operation.StatusAwaiting
			}
			u.Viewer.Rows = []viewer.Row{
				{ID: s.ID, Runtime: viewer.RuntimeRunning},
				{ID: "lead", ParentID: s.ID, Runtime: tc.runtime, ParentOperationStatus: leadStatus, Label: "lead"},
				{ID: "worker", ParentID: "lead", Runtime: viewer.RuntimeUnknown, ParentOperationStatus: tc.status, Finish: tc.finish, Label: "worker", Depth: 2},
			}
			rows := children(u, s)
			if got := childVisualState(rows[1], s); got != tc.want {
				t.Fatalf("grandchild state=%v, want %v", got, tc.want)
			}
			if got := workingChildren(s, u); got != tc.working {
				t.Fatalf("workingChildren=%d, want %d", got, tc.working)
			}
			o := BuildOrchestration(s, u.Viewer, u.Now, 128)
			headerText := header(s, u, makeLayout(160, 30)).plain()
			dockText := plainRows(childDock(s, u, makeLayout(80, 30), false))
			if tc.working == 0 {
				if strings.Contains(headerText, "裏で作業中 子") || strings.Contains(o.cards[0].now, "作業中") || dockText != "" {
					t.Fatalf("stopped child counted: header=%q main=%q dock=%q", headerText, o.cards[0].now, dockText)
				}
				if len(o.cards) != 1 || len(o.beads) != 2 {
					t.Fatalf("stopped children retained as cards: cards=%+v beads=%+v", o.cards, o.beads)
				}
			} else if !strings.Contains(headerText, "裏で作業中 子2") || !strings.Contains(o.cards[0].now, "子 2件 作業中") || !strings.Contains(dockText, "作業中 2") || len(o.cards) != 3 {
				t.Fatalf("live grandchildren hidden: header=%q main=%q dock=%q cards=%+v", headerText, o.cards[0].now, dockText, o.cards)
			}
		})
	}
}

// An unknown parent (not yet observed, or re-syncing) does not end its
// children's display; old records of stopped leads are settled by the Host.
func TestUnknownParentKeepsRunningChildWorking(t *testing.T) {
	s, u := idleSnapshot(), plainUI()
	u.Viewer.Rows = []viewer.Row{
		{ID: s.ID, Runtime: viewer.RuntimeRunning},
		{ID: "lead", ParentID: s.ID, Runtime: viewer.RuntimeUnknown, ParentOperationStatus: operation.StatusAwaiting, Label: "lead"},
		{ID: "worker", ParentID: "lead", Runtime: viewer.RuntimeUnknown, ParentOperationStatus: operation.StatusAwaiting, Label: "worker", Depth: 2},
	}
	rows := children(u, s)
	if got := childVisualState(rows[1], s); got != stateWorking {
		t.Fatalf("grandchild under an unknown parent = %v, want working", got)
	}
}

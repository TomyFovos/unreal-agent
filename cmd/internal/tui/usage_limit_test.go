package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
)

func TestUsageLimitWaitIsExplicitForMainAndChild(t *testing.T) {
	s, u := idleSnapshot(), plainUI()
	until := u.Now.Add(43 * time.Hour)
	s.Progress = &host.Progress{Mode: "usage_limit_wait", RetryAt: until}
	want := llm.UsageLimitWaiting(until)
	if got := mainCardPulse(s, viewer.Row{}, nil, u.Now); got != want {
		t.Fatal(got)
	}
	if got := pulse(s, u, makeLayout(180, 40)).plain(); !strings.Contains(got, want) {
		t.Fatal(got)
	}
	r := viewer.Row{ID: "child", ParentID: s.ID, ParentOperationStatus: operation.StatusAwaiting, UsageWaitUntil: until, LastActivityAt: u.Now.Add(-time.Hour), Cursor: 1}
	if got := childVisualState(r, s); got != stateUsageWait || childStalled(r, s, u.Now) {
		t.Fatal("waiting appears busy or stalled", got)
	}
	_, label, _, _ := childState(r, s, u.Theme)
	if label != want {
		t.Fatal(label)
	}
	cards, _ := buildWorkCards(s, viewer.PanelSnapshot{Rows: []viewer.Row{{ID: s.ID}, r}}, u.Now)
	if len(cards) != 2 || cards[1].state != stateUsageWait || cards[1].now != want {
		t.Fatal(cards)
	}
}

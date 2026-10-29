package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
)

func TestBackgroundActivitySpinsInHeaderChatAndMainCard(t *testing.T) {
	s, u := idleSnapshot(), plainUI()
	u.Theme.Still = false
	u.Viewer = viewer.PanelSnapshot{Version: 1, Rows: []viewer.Row{{ID: s.ID}, {ID: "child", ParentID: s.ID, ParentOperationStatus: operation.StatusAwaiting}}}
	s.Version = 1
	u.cache = &bodyCache{}
	for _, g := range header(s, u, makeLayout(160, 30)) {
		if g.text == "裏" && g.style != visualStyle(bandOK) {
			t.Fatal("background band is not ok")
		}
	}

	for _, width := range []int{80, 160} {
		first := RenderFrame(s, u, width, 30)
		spin := u.Theme.spinner(u.Now.UnixMilli())
		if !strings.Contains(first.Lines[0], spin+" 裏で作業中 子1") || strings.Count(frameText(first), spin+" 裏で作業中 子1") < 2 {
			t.Fatal("child activity is invisible", frameText(first))
		}
		if width >= 120 && !strings.Contains(frameText(first), spin+" 待機中（子 1件 作業中）") {
			t.Fatal("main card does not spin", frameText(first))
		}
		later := u
		later.Now = u.Now.Add(120 * time.Millisecond)
		next := RenderFrame(s, later, width, 30)
		if first.Lines[0] == next.Lines[0] {
			t.Fatal("cached spinner stopped")
		}
		writeVisualFrame(t, first, "background-child-"+number(int64(width)))
	}
	for _, phase := range []string{"waiting", "streaming", "tools"} {
		s.Entries = []Entry{{Role: "peer", PeerID: "child", Text: "finished"}, {Role: "agent", Text: "hidden work", TurnID: "background"}}
		s.Progress = &host.Progress{Mode: phase}
		if phase == "streaming" {
			s.Progress.Text = "処理中"
		}
		if phase == "tools" {
			s.Progress = nil
			s.Operations = []operation.Operation{{ID: "tool", ToolName: "Bash", Status: operation.StatusAwaiting}}
		}
		s.Version++
		f := RenderFrame(s, u, 80, 24)
		spin := u.Theme.spinner(u.Now.UnixMilli())
		if !strings.Contains(frameText(f), spin+" 裏の作業") || !strings.Contains(frameText(f), spin+" 裏で作業中") {
			t.Fatal(phase, frameText(f))
		}
		later := u
		later.Now = u.Now.Add(120 * time.Millisecond)
		next := RenderFrame(s, later, 80, 24)
		if !strings.Contains(frameText(next), later.Theme.spinner(later.Now.UnixMilli())+" 裏の作業") {
			t.Fatal("aggregate spinner cache", frameText(next))
		}
	}
}

func TestIdleHeaderIsNeutralInputReady(t *testing.T) {
	s, u := idleSnapshot(), plainUI()
	row := header(s, u, makeLayout(80, 24))
	if !strings.Contains(row.plain(), "入力待ち") || row[len(row)-1].style != visualStyle(bandNeutral) || pulse(s, u, makeLayout(80, 24)) != nil {
		t.Fatal(row.plain())
	}
}

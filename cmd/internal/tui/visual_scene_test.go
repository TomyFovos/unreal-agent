package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rivo/uniseg"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
)

// A shared scene for the chat, cards, header, attention band and gauges.
func visualWorkScene() (Snapshot, UIState) {
	s, u := idleSnapshot(), plainUI()
	u.Now = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	u.Theme, u.View = Theme{Depth: ColorTrue, Still: true}, ViewSplit
	s.ID, s.LatestAt = "jh", u.Now.Add(-6*time.Second)
	s.Selection = &sessionstore.RuntimeSelection{Provider: "claude-code", Model: "opus", Name: "Claude Opus 5.5", Effort: "max", ContextWindow: 1_000_000}
	s.LastGeneration = &llm.GenerationStatistics{Model: "opus", Source: "client_clock", DurationMicros: 1_000_000, Tokens: llm.TokenCounts{Input: 394_000, InputKnown: true, Output: 69, OutputKnown: true}}
	for _, output := range []int64{18, 29, 45, 78, 68, 55, 36} {
		stats := *s.LastGeneration
		stats.Tokens.Output = output
		s.SpeedHistory = append(s.SpeedHistory, stats)
	}
	s.Progress = &host.Progress{Mode: "streaming", Statistics: s.LastGeneration}
	s.CacheLatest = viewer.Usage{Known: true, Input: 394_000, CachedInput: 386_120}
	s.Permissions.Enabled, s.Permissions.Mode = true, permission.FullAccess
	s.Entries = []Entry{{Role: "you", At: u.Now.Add(-10 * time.Minute), Text: "決定を反映して、実装とレビューを進めて"}, {Role: "agent", Text: "実装担当と調査担当に作業を渡しました。"}}
	u.Viewer = viewer.PanelSnapshot{ParentID: s.ID, Selected: s.ID}
	main := viewer.Row{ID: s.ID, Generation: "main-generation", CreatedAt: u.Now.Add(-7*time.Hour - 45*time.Minute), Elapsed: viewer.Duration{Known: true, Value: 7*time.Hour + 45*time.Minute}, Selection: s.Selection, Usage: viewer.Usage{Known: true, Input: 144_400_000, Output: 738_800}}
	main.RecentActivity = sceneActivity(u.Now, []int{2, 3, 5, 3, 2, 1, 2, 3, 5, 3})
	u.Viewer.Rows = append(u.Viewer.Rows, main)
	titles := []string{"T1：Stack と Queue の実装（境界値・fixture）", "独立レビュー（設計）", "調査：文字列関数の仕様", "公開準備：README の修正4件", "T3: fix full test temporary directory", "作業パッケージ T2「テストの整理」", "A2: Queue のテスト追加"}
	for i, title := range titles {
		r := viewer.Row{ID: session.ID(fmt.Sprintf("internal-child-%02d", i)), ParentID: s.ID, ParentOperationID: operation.ID(fmt.Sprintf("internal-spawn-%02d", i)), ParentOperationStatus: operation.StatusAwaiting, Label: title, Depth: 1, Generation: "observed", CreatedAt: u.Now.Add(-75*time.Minute + time.Duration(i)*time.Minute), LastActivityAt: u.Now.Add(-time.Second), Elapsed: viewer.Duration{Known: true, Value: (75 - time.Duration(i)) * time.Minute}, Selection: &sessionstore.RuntimeSelection{Provider: "openai-codex", Model: "gpt-6.1-sol", Effort: "high"}}
		r.RecentActivity = sceneActivity(u.Now, []int{2, 3, 5, 8, 5, 3, 2, 5, 8, 5})
		r.Operations = []viewer.OperationRow{{ID: "internal-tool", Tool: "claude_code", Target: "Bash go test ./harness/...", Status: operation.StatusReady, Sequence: 30}}
		if i == 2 {
			r.LastActivityAt = u.Now.Add(-8 * time.Minute)
			r.RecentActivity = sceneActivity(u.Now, []int{5, 3, 0, 0, 0, 0, 0, 0, 0, 0})
		}
		if i == 5 {
			r.ParentOperationStatus = operation.StatusFailed
			r.Finish = &viewer.Finish{Status: "failure", RecordedAt: u.Now.Add(-20 * time.Minute)}
			r.Operations, r.RecentActivity = nil, nil
		}
		u.Viewer.Rows = append(u.Viewer.Rows, r)
	}
	for i, parent := range []session.ID{"internal-child-00", "internal-child-05"} {
		r := viewer.Row{ID: session.ID(fmt.Sprintf("internal-grandchild-%02d", i)), ParentID: parent, ParentOperationStatus: operation.StatusAwaiting, Label: []string{"孫：独立レビュー（拒否系）", "孫：Issue完了照合を継続"}[i], Depth: 2, Generation: "observed", CreatedAt: u.Now.Add(-12 * time.Minute), LastActivityAt: u.Now.Add(-time.Second), Elapsed: viewer.Duration{Known: true, Value: 12 * time.Minute}, Selection: &sessionstore.RuntimeSelection{Model: "gpt-6.1-sol", Effort: "medium"}, Activity: "tool: Read AGENTS.md"}
		r.RecentActivity = sceneActivity(u.Now, []int{0, 1, 2, 3, 2, 1, 2, 3, 5, 2})
		u.Viewer.Rows = append(u.Viewer.Rows, r)
	}
	// Creation follows the nine live lineages so their marks remain A through I.
	for i := range 800 {
		at := u.Now.Add(-11*time.Minute + time.Duration(i)*time.Second/2)
		u.Viewer.Rows = append(u.Viewer.Rows, viewer.Row{ID: session.ID(fmt.Sprintf("internal-finished-%03d", i)), ParentID: s.ID, ParentOperationStatus: operation.StatusCompleted, Label: "終了した作業", CreatedAt: at.Add(-time.Second), Finish: &viewer.Finish{Status: "completed", RecordedAt: at}})
	}
	// The last user input follows all completed children; only the failed parent
	// with an active descendant remains a failure card.
	s.Entries[0].At = u.Now.Add(-4 * time.Minute)
	s.Entries[1].TurnID = "scene-human-turn"
	s.Entries[1].Calls = []Receipt{
		{Name: "claude_code", Target: "Read AGENTS.md", Status: operation.StatusCompleted},
		{Name: "claude_code", Target: "Write AGENTS.md", Status: operation.StatusCompleted},
		{Name: "claude_code", Target: "Bash first", Status: operation.StatusCompleted},
		{Name: "claude_code", Target: "Bash second", Status: operation.StatusCompleted},
		{Name: "claude_code", Target: "Bash third", Status: operation.StatusCompleted},
		{Name: "claude_code", Target: "Bash go test ./...", Status: operation.StatusReady, StartedAt: u.Now.Add(-12 * time.Second)},
		{Name: "claude_code", Target: "Edit render.go", Status: operation.StatusFailed, Reason: "old_string not found in file"},
		{Name: "Bash", Target: "rm -rf build-cache", Status: operation.StatusAwaiting, Operations: []operation.ID{"internal-approval"}},
	}
	s.Permissions.Pending = []permission.Pending{{ID: "internal-approval", Tool: "Bash"}}
	payload := []byte(`{"Mode":"dispatch_operation","Parameters":{"OperationID":"internal-spawn-00","ToolName":"SubagentStart","Spec":{}}}`)
	s.Entries = append(s.Entries, entriesFor(host.HistoryItem{RecordedAt: u.Now.Add(-time.Minute), Data: inbox.Input{Kind: inbox.InputControl, Payload: payload}})...)
	s.Entries = append(s.Entries,
		Entry{Role: "peer", PeerID: "internal-child-00", PeerKind: "message", Text: "裏の詳細は表示しない"},
		Entry{Role: "agent", TurnID: "scene-background-1", Text: "作業を継続します。", Calls: []Receipt{{Name: "Read", Status: operation.StatusCompleted}, {Name: "Bash", Status: operation.StatusCompleted}}},
		Entry{Role: "agent", TurnID: "scene-background-1", Calls: []Receipt{{Name: "Write", Status: operation.StatusCompleted}}},
		Entry{Role: "peer", PeerID: "internal-child-02", PeerKind: "ended", Text: "子の終了通知"},
		Entry{Role: "agent", TurnID: "scene-background-2", Text: "結果を整理しています。"},
		Entry{Role: "agent", TurnID: "scene-attention", Text: "【要確認】失敗した編集の扱いを確認してください。\nほかの作業は継続しています。"})
	return s, u
}

func sceneActivity(now time.Time, counts []int) []time.Time {
	var activity []time.Time
	for i, n := range counts {
		for range n {
			activity = append(activity, now.Add(-10*time.Minute+time.Duration(i)*time.Minute+30*time.Second))
		}
	}
	return activity
}

func writeVisualFrame(t *testing.T, frame Frame, name string) {
	t.Helper()
	if os.Getenv("UNREAL_AGENT_WRITE_VISUAL") != "1" {
		return
	}
	path := filepath.Join("testdata", "visual", name+".ans")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(frame.Lines, "\n")+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Log("wrote", path)
}

func TestVisualWorkPaneFrames(t *testing.T) {
	s, u := visualWorkScene()
	o := BuildOrchestration(s, u.Viewer, u.Now, 128)
	if len(o.cards) != 10 || len(o.beads) != 800 {
		t.Fatalf("scene cards=%d beads=%d", len(o.cards), len(o.beads))
	}
	failed, stalled, grandchildren := 0, 0, 0
	for _, c := range o.cards {
		if c.state == stateFailed {
			failed++
		}
		if c.stalled {
			stalled++
		}
		if c.depth == 1 {
			grandchildren++
		}
	}
	if failed != 1 || stalled != 1 || grandchildren != 2 {
		t.Fatalf("scene failed=%d stalled=%d grandchildren=%d", failed, stalled, grandchildren)
	}
	chat := chatText(s, u, 215)
	for _, want := range []string{"✓ Read AGENTS.md", "✓ Write AGENTS.md", "✓ Bash ×3", "▸ Bash go test ./... 12s", "✗ Edit render.go 失敗", "old_string not found in file", "! Bash rm -rf build-cache 承認待ち", "▸ [A] T1", "┄ 裏の作業  2ターン · ツール3回 · 報告 [A][C]", "【要確認】", "ほかの作業は継続しています。"} {
		if !strings.Contains(chat, want) {
			t.Fatalf("chat scene missing %q:\n%s", want, chat)
		}
	}
	if len(s.Permissions.Pending) != 1 || strings.Count(chat, "【要確認】") != 1 || strings.Count(chat, "裏の作業") != 1 {
		t.Fatal("scene must contain one approval, one attention report and one background group")
	}
	for _, size := range [][2]int{{215, 50}, {160, 40}, {120, 30}, {80, 24}, {45, 20}} {
		f := RenderFrame(s, u, size[0], size[1])
		writeVisualFrame(t, f, fmt.Sprintf("work-pane-%dx%d", size[0], size[1]))
		header := sgrRE.ReplaceAllString(f.Lines[0], "")
		band := sgrRE.ReplaceAllString(f.Lines[size[1]-5], "")
		gauge := sgrRE.ReplaceAllString(f.Lines[size[1]-1], "")
		if !strings.Contains(header, "裏で作業中 子8") || !strings.Contains(band, "承認待ち 1件") || !strings.Contains(band, "+1") || !strings.Contains(gauge, "文脈") || !strings.Contains(gauge, "394k (39%)") || !strings.Contains(gauge, "~69.0 tok/s") {
			t.Fatal("scene header/band/gauge missing", size, header, band, gauge)
		}
		if size[0] >= 120 && (!strings.Contains(gauge, "98.00%") || strings.Contains(gauge, "累計") || !strings.Contains(header, "Claude Opus 5.5 · max")) {
			t.Fatal("wide scene lacks model/cache/usage", size, header, gauge)
		}
		for _, view := range []ViewMode{ViewSplit, ViewChat, ViewOrchestration} {
			for _, theme := range []Theme{{Depth: ColorTrue}, {Depth: Color256}, {Depth: Color16}, {NoColor: true}, {Plain: true}, {Depth: Color16, ASCII: true}} {
				u.View, u.Theme = view, theme
				frame := RenderFrame(s, u, size[0], size[1])
				exportFrameBounds(t, frame, size[0], size[1])
				text := frameText(frame)
				for i, row := range frame.Lines {
					if width := uniseg.StringWidth(sgrRE.ReplaceAllString(row, "")); width > size[0]-1 {
						t.Fatalf("view=%s size=%v theme=%+v row=%d width=%d", view, size, theme, i, width)
					}
				}
				for _, gone := range []string{"internal-child-", "internal-grandchild-", "internal-finished-", "internal-spawn-", "internal-tool", "internal-approval", "control:", "host control", "裏の詳細は表示しない", "r0", "turn:", "observed:", "Host validates", "owns requests", "active path", "queued"} {
					if strings.Contains(text, gone) {
						t.Fatalf("%q exposed in %s %v", gone, view, size)
					}
				}
				if theme.ASCII && strings.ContainsAny(text, "▌▎●◆▁▂▃▄▅▆▇█░▕▏✓✗⊘▸") {
					t.Fatalf("Unicode card symbols in ASCII: %s", text)
				}
				if theme.Plain && sgrRE.MatchString(text) {
					t.Fatal("plain frame emitted SGR")
				}
			}
		}
		u.View, u.Theme = ViewSplit, Theme{Depth: ColorTrue, Still: true}
	}
}

func TestChildRailUsesSharedCards(t *testing.T) {
	s, u := visualWorkScene()
	l := layout{rail: 48, height: 40}
	got := childRail(s, u, l, 32)
	want, _, _ := workCardPane(BuildOrchestration(s, u.Viewer, u.Now, 320), u, 48, 32, true)
	if plainRows(got) != plainRows(want) {
		t.Fatalf("Chat rail differs from cards:\n%s\nwant:\n%s", plainRows(got), plainRows(want))
	}
}

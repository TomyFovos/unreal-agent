package tui

import (
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
)

func chatText(s Snapshot, u UIState, width int) string {
	l := makeLayout(width, 100)
	var rows []string
	for _, row := range conversation(s, u, l) {
		rows = append(rows, l.row(row).plain())
	}
	return strings.Join(rows, "\n")
}

func TestChatToolChipsExpandGroupAndWrap(t *testing.T) {
	s, u := idleSnapshot(), plainUI()
	e := Entry{Calls: []Receipt{
		{Name: "claude_code", Target: "Read AGENTS.md", Status: operation.StatusCompleted},
		{Name: "claude_code", Target: "Bash first", Status: operation.StatusCompleted},
		{Name: "Bash", Target: "second", Status: operation.StatusCompleted},
		{Name: "claude_code", Target: "Bash third", Status: operation.StatusCompleted},
		{Name: "claude_code", Target: "Bash go test ./...", Status: operation.StatusReady, StartedAt: u.Now.Add(-12 * time.Second)},
		{Name: "Bash", Target: "still running", Status: operation.StatusAwaiting, StartedAt: u.Now.Add(-80 * time.Second)},
		{Name: "claude_code", Target: "Edit render.go", Status: operation.StatusFailed, Reason: "old_string not found in file\nsecond reason line"},
		{Name: "Edit", Target: "other.go", Status: operation.StatusFailed, Reason: "second failure"},
	}}
	rows := toolChips(e, s, u, makeLayout(80, 30))
	var text []string
	for _, row := range rows {
		if row.text.width() > 77 {
			t.Fatalf("chip row width=%d", row.text.width())
		}
		text = append(text, row.text.plain())
	}
	got := strings.Join(text, "\n")
	for _, want := range []string{"[ ✓ Read AGENTS.md ] [ ✓ Bash ×3 ]", "[ ▸ Bash go test ./... 12s ]", "[ ▸ Bash still running 1m ]", "[ ✗ Edit render.go 失敗 ]\nold_string not found in file", "[ ✗ Edit other.go 失敗 ]\nsecond failure"} {
		if strings.Count(got, want) != 1 {
			t.Fatalf("want once %q:\n%s", want, got)
		}
	}
	for _, gone := range []string{"claude_code", "first", "second reason line", "third", "×5", "queued"} {
		if strings.Contains(got, gone) {
			t.Fatalf("unexpected %q:\n%s", gone, got)
		}
	}
	// Two calls keep both targets; grouping is per expanded name, not all calls.
	e.Calls = []Receipt{{Name: "read", Target: "one.go", Status: operation.StatusCompleted}, {Name: "read", Target: "two.go", Status: operation.StatusCompleted}, {Name: "write", Target: "three.go", Status: operation.StatusCompleted}}
	rows = toolChips(e, s, u, makeLayout(100, 30))
	if len(rows) != 1 || rows[0].text.plain() != "[ ✓ read one.go ] [ ✓ read two.go ] [ ✓ write three.go ]" {
		t.Fatalf("grouped different tools or two calls: %+v", rows)
	}
}

func TestChatToolChipWidthsStylesAndTruncation(t *testing.T) {
	s, u := idleSnapshot(), plainUI()
	for _, theme := range []Theme{{Depth: ColorTrue}, {Depth: Color256}, {Depth: Color16}, {NoColor: true}, {Plain: true}, {ASCII: true}} {
		u.Theme = theme
		for _, name := range []string{"Bash", "Read", "SubagentStart", strings.Repeat("長い名前", 20)} {
			target := strings.Repeat("日本語👩🏽‍💻é/", 30) + "render.go"
			for _, width := range []int{40, 100} {
				chip, _ := receiptChip(Receipt{Name: name, Target: target, Status: operation.StatusCompleted}, viewer.Duration{}, s, u, width, nil, nil)
				if chip.width() > 40 || !strings.Contains(chip.plain(), theme.symbol("…", "~")) {
					t.Fatalf("name=%s theme=%+v width=%d: %s", name, theme, chip.width(), chip.plain())
				}
				if name == "Read" && !strings.Contains(chip.plain(), "render.go") {
					t.Fatalf("lost basename: %s", chip.plain())
				}
				if name == "Bash" && strings.Contains(chip.plain(), "render.go") {
					t.Fatalf("command middle-truncated: %s", chip.plain())
				}
			}
		}
		chip, reason := receiptChip(Receipt{Name: "Edit", Target: "render.go", Status: operation.StatusFailed, Reason: strings.Repeat("理由", 80) + "\nnot shown"}, viewer.Duration{}, s, u, 40, nil, nil)
		if len(strings.Split(reason, "\n")) != 1 || textLine(reason, normal).width() > 40 || !strings.HasSuffix(reason, theme.symbol("…", "~")) {
			t.Fatalf("reason not one tail-truncated line: %q", reason)
		}
		foundMark := false
		for _, g := range chip {
			if g.style.bg != chipBad {
				t.Fatalf("missing failure face: %+v", g)
			}
			if g.text == theme.symbol("✗", "x") {
				foundMark = g.style.fg == stateBad
			}
		}
		if !foundMark {
			t.Fatal("failure mark lost state color")
		}
	}
}

func TestChatApprovalChipRequiresAwaitingAndPendingID(t *testing.T) {
	s, u := idleSnapshot(), plainUI()
	s.Permissions.Pending = []permission.Pending{{ID: "approval"}}
	for _, tc := range []struct {
		status operation.Status
		id     operation.ID
		want   string
	}{
		{operation.StatusAwaiting, "approval", "[ ! Bash rm -rf tmp 承認待ち ]"},
		{operation.StatusAwaiting, "other", "[ ▸ Bash rm -rf tmp ]"},
		{operation.StatusReady, "approval", "[ ▸ Bash rm -rf tmp ]"},
		{operation.StatusCanceling, "approval", "[ ▸ Bash rm -rf tmp 取消中 ]"},
		{operation.StatusCompleted, "approval", "[ ✓ Bash rm -rf tmp ]"},
	} {
		// Canonical operations supersede the old receipt state, including
		// canceling, which must not be flattened into ready/working.
		s.Operations = []operation.Operation{{ID: tc.id, Status: tc.status}}
		r, elapsed := currentReceipt(Receipt{Name: "Bash", Target: "rm -rf tmp", Status: operation.StatusCompleted, Operations: []operation.ID{tc.id}}, s, u)
		chip, _ := receiptChip(r, elapsed, s, u, 100, nil, nil)
		if chip.plain() != tc.want {
			t.Fatalf("status=%s id=%s: %q want %q", tc.status, tc.id, chip.plain(), tc.want)
		}
	}
}

func chatControl(t *testing.T, mode inbox.ControlMode, params any) host.HistoryItem {
	t.Helper()
	payload, err := json.Marshal(inbox.ControlMessage{Mode: mode, Reason: "display fixture", Parameters: params})
	if err != nil {
		t.Fatal(err)
	}
	return host.HistoryItem{Sequence: 2, RecordedAt: time.Unix(88, 0), Data: inbox.Input{Kind: inbox.InputControl, Payload: payload}}
}

func TestChatDispatchOperationLaunchChip(t *testing.T) {
	s, u := idleSnapshot(), plainUI()
	item := chatControl(t, inbox.DispatchOperation, inbox.OperationIntent{OperationID: "spawn-op", ToolName: "SubagentStart", Spec: []byte(`{"private":"not displayed"}`)})
	entries := entriesFor(item)
	if len(entries) != 1 || entries[0].Role != "agent" || entries[0].Text != "" || len(entries[0].Calls) != 1 {
		t.Fatalf("dispatch not projected as a receipt: %+v", entries)
	}
	r := entries[0].Calls[0]
	if r.Name != "SubagentStart" || len(r.Operations) != 1 || r.Operations[0] != "spawn-op" || r.StartedAt != item.RecordedAt {
		t.Fatalf("dispatch linkage lost: %+v", r)
	}
	u.Viewer.Rows = []viewer.Row{{ID: "child-uuid", ParentID: s.ID, ParentOperationID: "spawn-op", Label: "T1 Stack実装\nsecond title line", CreatedAt: time.Unix(1, 0), ParentOperationStatus: operation.StatusAwaiting, Selection: &sessionstore.RuntimeSelection{Model: "model-is-not-title"}}}
	s.Entries = append([]Entry{{Role: "agent", Text: "作業を起動します。"}}, entries...)
	for _, tc := range []struct {
		status operation.Status
		finish *viewer.Finish
		want   string
	}{
		{operation.StatusAwaiting, nil, "▸"},
		{operation.StatusCanceling, nil, "▸"},
		{operation.StatusCompleted, nil, "▪"},
		{operation.StatusCompleted, &viewer.Finish{Status: "success"}, "✓"},
		{operation.StatusCompleted, &viewer.Finish{Status: "failure"}, "✗"},
	} {
		u.Viewer.Rows[0].ParentOperationStatus, u.Viewer.Rows[0].Finish = tc.status, tc.finish
		got := chatText(s, u, 120)
		want := "● 作業を起動します。\n  [ " + tc.want + " [A] T1 Stack実装 ]"
		if got != want {
			t.Fatalf("launch chip: %q want %q", got, want)
		}
		for _, gone := range []string{"control:", "host control", "spawn-op", "child-uuid", "model-is-not-title", "second title line", "private"} {
			if strings.Contains(got, gone) {
				t.Fatalf("%q exposed: %s", gone, got)
			}
		}
	}
	// The badge remains on its own background inside the chip.
	rows := toolChips(entries[0], s, u, makeLayout(100, 30))
	var badge line
	for _, g := range rows[0].text {
		if g.style.bg == badgeColor {
			badge = append(badge, g)
		}
	}
	if badge.plain() != "[A]" {
		t.Fatalf("launch mark lost badge face: %q", badge.plain())
	}
}

func TestChatControlInputsJapaneseAndHidden(t *testing.T) {
	for _, tc := range []struct {
		mode   inbox.ControlMode
		params any
		want   string
	}{
		{inbox.StopHard, nil, "停止要求"}, {inbox.StopWhenIdle, nil, "停止要求"},
		{inbox.InterruptAgent, nil, "中断要求"}, {inbox.CancelChildren, nil, "子をすべて取消"},
		{inbox.CancelOperation, inbox.CancelRequest{OperationID: "op"}, "操作の取消"},
		{inbox.UpdateSettings, inbox.Settings{ReasoningEffort: "high"}, "推論の深さを変更: high"},
		{inbox.Heartbeat, nil, ""},
	} {
		s, u := idleSnapshot(), plainUI()
		s.Entries = entriesFor(chatControl(t, tc.mode, tc.params))
		want := ""
		if tc.want != "" {
			want = "· " + tc.want
		}
		if got := chatText(s, u, 80); got != want {
			t.Fatalf("control %s: %q want %q", tc.mode, got, want)
		}
	}
}

func TestChatBackgroundCountsMarksAndBoundaries(t *testing.T) {
	s, u := idleSnapshot(), plainUI()
	s.Entries = []Entry{{Role: "you", Text: "開始"}}
	// A child that only announced it is ready has not reported.
	u.Viewer.Rows = append(u.Viewer.Rows, viewer.Row{ID: "private-child-ready", ParentID: s.ID, CreatedAt: time.Unix(100, 0)})
	s.Entries = append(s.Entries, Entry{Role: "peer", PeerID: "private-child-ready", PeerKind: "ready", Text: "hidden ready"})
	for i := range 10 {
		id := session.ID(fmt.Sprintf("private-child-%02d", i))
		u.Viewer.Rows = append(u.Viewer.Rows, viewer.Row{ID: id, ParentID: s.ID, CreatedAt: time.Unix(int64(i), 0)})
		s.Entries = append(s.Entries,
			Entry{Role: "peer", PeerID: string(id), PeerKind: "message", Text: "hidden child detail"},
			Entry{Role: "agent", TurnID: session.TurnID(fmt.Sprintf("turn-%d", i/2)), Text: "hidden agent detail", Calls: []Receipt{{Name: "Bash"}, {Name: "Read"}}},
			Entry{Role: "peer", PeerID: string(id), PeerKind: "ended", Text: "hidden ended detail"})
	}
	// Repeated sender and a call-only dispatch contribute receipts, not a new turn.
	s.Entries = append(s.Entries, Entry{Role: "peer", PeerID: "private-child-00"}, Entry{Role: "agent", Calls: []Receipt{{Name: "SubagentStart"}}})
	got := chatText(s, u, 120)
	want := "┄ 裏の作業  5ターン · ツール21回 · 報告 [A][B][C][D][E][F][G][H]+2"
	if strings.Count(got, "裏の作業") != 1 || !strings.Contains(got, want) || strings.Contains(got, "hidden") || strings.Contains(got, "private-child") {
		t.Fatalf("background aggregate:\n%s", got)
	}
	// Narrow rows keep whole badges, counting the marks that did not fit.
	var work backgroundWork
	for _, e := range s.Entries[1:] {
		work.add(e)
	}
	marks := childMarks(children(u, s))
	for _, theme := range []Theme{{Depth: ColorTrue}, {Depth: Color256}, {Depth: Color16}, {NoColor: true}, {Plain: true}, {ASCII: true}} {
		row := work.row(marks, 42, theme)
		want := theme.symbol("┄", "-") + " 裏の作業  5ターン · ツール21回 +10"
		if row.plain() != want || row.width() > 42 {
			t.Fatalf("narrow background row=%q width=%d want=%q", row.plain(), row.width(), want)
		}
		// Three more cells fit the report label and one whole badge with +9.
		row = work.row(marks, 45, theme)
		want = theme.symbol("┄", "-") + " 裏の作業  5ターン · ツール21回 · 報告 [A]+9"
		if row.plain() != want || row.width() > 45 {
			t.Fatalf("narrow background badge=%q width=%d want=%q", row.plain(), row.width(), want)
		}
	}
	// The final group updates as canonical receipts/turns arrive.
	s.Entries = append(s.Entries, Entry{Role: "agent", TurnID: "new-turn", Calls: []Receipt{{Name: "Bash"}}})
	got = chatText(s, u, 120)
	if strings.Count(got, "裏の作業") != 1 || !strings.Contains(got, "6ターン · ツール22回") {
		t.Fatalf("final aggregate did not update:\n%s", got)
	}
	s.Entries = append(s.Entries,
		Entry{Role: "agent", Text: "【報告】全文その1\n全文その2"},
		Entry{Role: "agent", TurnID: "after-report", Text: "hidden"},
		Entry{Role: "agent", Text: "【要確認】全文その3\n全文その4"},
		Entry{Role: "agent", TurnID: "after-attention", Text: "hidden"},
		Entry{Role: "error", Text: "エラー全文その5\n全文その6", Code: "writer_owned"},
		Entry{Role: "agent", TurnID: "after-error", Text: "hidden"},
		Entry{Role: "you", Text: "続けて"}, Entry{Role: "agent", Text: "ユーザーへの全文その7\n全文その8"})
	got = chatText(s, u, 120)
	if strings.Count(got, "裏の作業") != 4 {
		t.Fatalf("human/error/user boundaries not separated:\n%s", got)
	}
	last := -1
	for _, want := range []string{"6ターン · ツール22回", "● 【報告】全文その1\n  全文その2", "1ターン · ツール0回", "● 【要確認】全文その3\n  全文その4", "1ターン · ツール0回", "✗ 別の画面で使用中です。その画面を閉じてから /resume してください。", "1ターン · ツール0回", "▎ 続けて", "● ユーザーへの全文その7\n  全文その8"} {
		i := strings.Index(got[last+1:], want)
		if i < 0 {
			t.Fatalf("missing/out-of-order %q:\n%s", want, got)
		}
		last += 1 + i + len(want) - 1
	}
	if strings.Contains(got, "hidden") {
		t.Fatalf("background text leaked:\n%s", got)
	}
}

func TestChatGuttersBandsDraftAndComposer(t *testing.T) {
	s, u := idleSnapshot(), plainUI()
	s.Entries = []Entry{{Role: "you", Text: "あなたの発言\n二行目"}, {Role: "agent", Text: "最初の応答\n次の行"}, {Role: "agent", Text: "連続する応答"}, {Role: "error", Text: "エラー"}}
	s.Progress = &host.Progress{Mode: "streaming", Text: "下書き\n二行目"}
	got := chatText(s, u, 80)
	for _, want := range []string{"▎ あなたの発言", "▎ 二行目", "● 最初の応答\n  次の行", "  連続する応答", "✗ 応答を取得できませんでした", "⋮ 下書き\n⋮ 二行目"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q:\n%s", want, got)
		}
	}
	if strings.Count(got, "●") != 1 || strings.Contains(got, "you") || strings.Contains(got, "agent") {
		t.Fatalf("role label or repeated agent marker:\n%s", got)
	}
	l := makeLayout(80, 24)
	rows := conversation(s, u, l)
	for _, row := range rows {
		if row.role != "you" {
			continue
		}
		if row.content.width() != l.text {
			t.Fatal("user band not padded to body width")
		}
		for _, g := range row.content {
			if g.style.bg != youBlock || g.style.fg != colorDefault {
				t.Fatalf("user band missing paired color: %+v", g)
			}
		}
	}
	for _, tc := range []struct {
		input  string
		pasted bool
		want   string
		cursor int
	}{
		{"猫👩🏽‍💻", false, "› 猫👩🏽‍💻", 6},
		{"/view", false, "[ cmd ] › /view", 15},
		{"/view", true, "› /view", 7},
	} {
		u = plainUI()
		u.Input, u.Cursor, u.Pasted = tc.input, len(tc.input), tc.pasted
		rows, _, cursor, _, _ := composer(s, u, l)
		if rows[0].plain() != tc.want || cursor != tc.cursor {
			t.Fatalf("input=%q: %q cursor=%d want=%q %d", tc.input, rows[0].plain(), cursor, tc.want, tc.cursor)
		}
	}
}

func TestChatPulseExpandsWrapperAndUsesMainGutter(t *testing.T) {
	s, u := idleSnapshot(), plainUI()
	s.Operations = []operation.Operation{{ID: "op", ToolName: "claude_code", Status: operation.StatusReady}}
	u.Parent = &viewer.Row{Operations: []viewer.OperationRow{{ID: "op", Tool: "claude_code", Target: "Bash go test ./...", Elapsed: viewer.Duration{Known: true, Value: 12 * time.Second}}}}
	row := pulse(s, u, makeLayout(80, 24))
	if row.plain() != u.Theme.spinner(u.Now.UnixMilli())+" Bash 実行中  12s" || row[0].style.fg != stateMain {
		t.Fatalf("pulse wrapper/gutter: %q %+v", row.plain(), row[0].style)
	}
}

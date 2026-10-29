package tui

import (
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
)

func TestVisualStatePresentation(t *testing.T) {
	for _, tc := range []struct {
		state             visualState
		mark, ascii, word string
		role              colorRole
		ended             bool
	}{
		{stateWorking, "▸", ">", "作業中", stateRun, false},
		{stateCanceling, "▸", ">", "取消中", stateStall, false},
		{stateApproval, "!", "!", "承認待ち", stateStall, false},
		{stateCompleted, "✓", "+", "完了", stateDone, true},
		{stateFailed, "✗", "x", "失敗", stateBad, true},
		{stateCanceled, "⊘", "-", "取消", dimColor, true},
		{stateEnded, "▪", "#", "終了", dimColor, true},
		{stateUnknown, "?", "?", "不明", stateStall, false},
	} {
		for _, theme := range []Theme{{}, {ASCII: true}} {
			mark, word, style := tc.state.presentation(theme)
			wantMark := tc.mark
			if theme.ASCII {
				wantMark = tc.ascii
			}
			if mark != wantMark || word != tc.word || style != visualStyle(tc.role) || tc.state.ended() != tc.ended {
				t.Fatalf("state %d: %q %q %+v ended=%v", tc.state, mark, word, style, tc.state.ended())
			}
			if strings.Contains(mark+word, "queued") {
				t.Fatal("visual state returned queued")
			}
		}
	}
}

func TestChildVisualStateTranslatesParentAndFinish(t *testing.T) {
	s := Snapshot{Connected: true}
	s.Permissions.Pending = []permission.Pending{{ID: "child-op"}}
	for _, tc := range []struct {
		status operation.Status
		want   visualState
	}{
		{operation.StatusAwaiting, stateWorking},
		{operation.StatusReady, stateWorking},
		{operation.StatusCanceling, stateCanceling},
		{operation.StatusCompleted, stateEnded},
		{operation.StatusFailed, stateFailed},
		{operation.StatusCanceled, stateCanceled},
		{"", stateUnknown},
	} {
		r := viewer.Row{ParentOperationID: "child-op", ParentOperationStatus: tc.status}
		if got := childVisualState(r, s); got != tc.want {
			t.Fatalf("parent %s = %d, want %d", tc.status, got, tc.want)
		}
	}
	for _, tc := range []struct {
		finish string
		want   visualState
	}{{"success", stateCompleted}, {"completed", stateCompleted}, {"failure", stateFailed}, {"failed", stateFailed}, {"error", stateFailed}, {"unexpected", stateUnknown}} {
		r := viewer.Row{ParentOperationStatus: operation.StatusCanceled, Finish: &viewer.Finish{Status: tc.finish}}
		if got := childVisualState(r, s); got != tc.want {
			t.Fatalf("Finish %q = %d, want %d", tc.finish, got, tc.want)
		}
	}
	for _, r := range []viewer.Row{{NeedsResync: true, ParentOperationStatus: operation.StatusAwaiting}, {NeedsResync: true, Finish: &viewer.Finish{Status: "success"}}} {
		if childVisualState(r, s) != stateUnknown {
			t.Fatal("resync must be unknown")
		}
	}
	if childVisualState(viewer.Row{ParentOperationStatus: operation.StatusAwaiting}, Snapshot{}) != stateUnknown {
		t.Fatal("disconnected child must be unknown")
	}
}

func TestToolVisualStateRequiresAwaitingAndMatchingApproval(t *testing.T) {
	s := Snapshot{Connected: true}
	s.Permissions.Pending = []permission.Pending{{ID: "other"}, {ID: "op-2"}}
	for _, tc := range []struct {
		status operation.Status
		ids    []operation.ID
		want   visualState
	}{
		{operation.StatusAwaiting, []operation.ID{"op-1", "op-2"}, stateApproval},
		{operation.StatusAwaiting, []operation.ID{"op-1"}, stateWorking},
		{operation.StatusAwaiting, nil, stateWorking},
		{operation.StatusReady, []operation.ID{"op-2"}, stateWorking},
		{operation.StatusCanceling, []operation.ID{"op-2"}, stateCanceling},
		{operation.StatusCompleted, []operation.ID{"op-2"}, stateCompleted},
		{operation.StatusFailed, nil, stateFailed},
		{operation.StatusCanceled, nil, stateCanceled},
		{"", nil, stateUnknown},
	} {
		r := Receipt{Status: tc.status, Operations: tc.ids}
		if got := toolVisualState(r, s); got != tc.want {
			t.Fatalf("tool %+v = %d, want %d", r, got, tc.want)
		}
		if got := toolVisualState(r, Snapshot{}); got != stateUnknown {
			t.Fatalf("disconnected tool = %d", got)
		}
	}
}

func TestDisplayToolExpandsClaudeCodeOnce(t *testing.T) {
	for _, tc := range []struct{ name, target, wantName, wantTarget string }{
		{"claude_code", "Bash go test ./...", "Bash", "go test ./..."},
		{"claude_code", "Read AGENTS.md", "Read", "AGENTS.md"},
		{"claude_code", "Write path with spaces.go", "Write", "path with spaces.go"},
		{"claude_code", "  Bash\t echo  'two  spaces'  ", "Bash", "echo  'two  spaces'"},
		{"claude_code", "Read\u3000日本語.go", "Read", "日本語.go"},
		{"claude_code", "Bash", "Bash", ""},
		{"claude_code", "  ", "", ""},
		{"Read", " file.go ", "Read", " file.go "},
	} {
		name, target := displayTool(tc.name, tc.target)
		if name != tc.wantName || target != tc.wantTarget {
			t.Fatalf("displayTool(%q, %q) = (%q, %q)", tc.name, tc.target, name, target)
		}
	}
}

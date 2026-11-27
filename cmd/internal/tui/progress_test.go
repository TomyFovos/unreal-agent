package tui

import (
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
)

func TestClaudeCodeUpdateWaitIsVisibleInTheTUI(t *testing.T) {
	s, u := idleSnapshot(), plainUI()
	s.Progress = &host.Progress{Mode: "claude_code_update_wait", Text: "waiting for Claude Code to finish updating"}
	const want = "Claude Code の更新を待っています"
	l := makeLayout(180, 40)
	if got := header(s, u, l).plain(); !strings.Contains(got, want) {
		t.Fatal(got)
	}
	if got := pulse(s, u, l).plain(); !strings.Contains(got, want) {
		t.Fatal(got)
	}
	if got := mainCardPulse(s, viewer.Row{}, nil, u.Now); got != want {
		t.Fatal(got)
	}
	s.Progress = &host.Progress{Mode: "streaming"}
	if got := pulse(s, u, l).plain(); strings.Contains(got, want) {
		t.Fatal(got)
	}
}

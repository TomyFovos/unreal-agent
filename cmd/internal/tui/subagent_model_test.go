package tui

import (
	"strings"
	"testing"
)

// Subagent settings are agent-facing CLI commands, with no TUI command or picker.
func TestSubagentModelCommandRemoved(t *testing.T) {
	for _, command := range commandCatalog {
		if strings.Contains(command.Name, "model-sub") || strings.Contains(command.Usage, "subagent model") {
			t.Fatalf("subagent model command in TUI catalog: %+v", command)
		}
	}
	if _, ok := submittedCommand("/model-sub"); ok {
		t.Fatal("removed command is still handled by the TUI")
	}
	if matches := candidates(Snapshot{}, UIState{Input: "/model-sub"}); len(matches) != 0 {
		t.Fatalf("removed command in completion: %+v", matches)
	}
	if help := strings.Join(helpSheet(Theme{Plain: true}, false).Lines, "\n"); strings.Contains(help, "model-sub") {
		t.Fatalf("removed command in help: %s", help)
	}
}

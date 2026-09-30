package contextbuilder

import (
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/projectinstructions"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

func systemText(t *testing.T, current Builder) string {
	t.Helper()
	result, err := current.Build()
	if err != nil {
		t.Fatal(err)
	}
	return result.Request.Input[0].Data.(llm.Message).Text
}

func TestBuilderLayersProjectInstructionsBetweenSystemPromptAndSkills(t *testing.T) {
	snapshot, err := projectinstructions.FromContent([]byte("\uFEFFUse tabs.\n"))
	if err != nil {
		t.Fatal(err)
	}
	current := NewBuilder(tool.Skill{Name: "review", Description: "Review code", Path: "/skills/review/SKILL.md"})
	if err = current.SetProjectInstructions(snapshot); err != nil {
		t.Fatal(err)
	}
	current.SetSystemPrompt("Be concise.")
	text := systemText(t, current)
	instructions := projectInstructionsPreamble + "\n\n<project_instructions source=\"AGENTS.md\">\nUse tabs.\n</project_instructions>"
	want := preamble + "\n\nBe concise.\n\n" + instructions + "\n\n" + formatSkillsForPrompt([]tool.Skill{{Name: "review", Description: "Review code", Path: "/skills/review/SKILL.md"}})
	if text != want {
		t.Fatalf("system prompt = %q, want %q", text, want)
	}
	// Lifecycle guidance edits only the harness layer.
	if err = current.(interface{ SetLifecycle(string) error }).SetLifecycle("interactive"); err != nil {
		t.Fatal(err)
	}
	if text = systemText(t, current); !strings.Contains(text, instructions) || strings.Count(text, "<project_instructions") != 1 {
		t.Fatalf("lifecycle rewrite lost or duplicated project instructions: %q", text)
	}
}

func TestBuilderOmitsEmptyProjectInstructions(t *testing.T) {
	blank, err := projectinstructions.FromContent([]byte(" \n\t"))
	if err != nil {
		t.Fatal(err)
	}
	for _, snapshot := range []projectinstructions.Snapshot{projectinstructions.None(), blank} {
		current := NewBuilder()
		if err = current.SetProjectInstructions(snapshot); err != nil {
			t.Fatal(err)
		}
		if text := systemText(t, current); text != preamble {
			t.Fatalf("system prompt = %q, want preamble only", text)
		}
	}
}

func TestBuilderRejectsInconsistentProjectInstructions(t *testing.T) {
	snapshot, err := projectinstructions.FromContent([]byte("A"))
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Content = "B"
	current := NewBuilder()
	if err = current.SetProjectInstructions(snapshot); err == nil {
		t.Fatal("tampered snapshot was accepted")
	}
	if text := systemText(t, current); text != preamble {
		t.Fatalf("rejected snapshot changed context: %q", text)
	}
}

package contextbuilder

import (
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/projectinstructions"
)

func TestTextOnlyBuilderKeepsInteractiveLifecycleAndProjectLayers(t *testing.T) {
	b := NewTextOnlyBuilder()
	b.SetSystemPrompt("Answer the user.")
	if err := b.(interface{ SetLifecycle(string) error }).SetLifecycle("interactive"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := projectinstructions.FromContent([]byte("Preserve project context."))
	if err != nil {
		t.Fatal(err)
	}
	if err := b.SetProjectInstructions(snapshot); err != nil {
		t.Fatal(err)
	}
	result, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	prompt := result.Request.Input[0].Data.(llm.Message).Text
	for _, want := range []string{"Tools, skills and child agents are unavailable", "Answer the user.", "Preserve project context.", "The host keeps the session alive until an explicit stop."} {
		if !strings.Contains(prompt, want) {
			t.Fatal("text-only prompt lost lifecycle or system guidance", want)
		}
	}
	for _, unwanted := range []string{textOnlyEnding, oneShotEnding, "Tool calls are asynchronous", "prefer to go wider with tool calls"} {
		if strings.Contains(prompt, unwanted) {
			t.Fatal("text-only interactive prompt claims unavailable behavior", unwanted)
		}
	}
	if len(result.Request.Tools) != 0 {
		t.Fatal("text-only construction supplied tool schemas")
	}
}

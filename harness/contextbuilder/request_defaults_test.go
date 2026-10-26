package contextbuilder

import (
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/projectinstructions"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

// Any accepted AGENTS.md (up to its 64 KiB limit, Japanese included) and a
// child's task fit the default budget of a model with a known window, as in
// Codex. The old fixed 24576-byte budget refused them before inference.
func TestLargeProjectInstructionsFitTheDefaultBudget(t *testing.T) {
	f := newContextFixture(t, "openai-codex", contextengine.Config{})
	f.b.SetContextRuntime(sessionstore.RuntimeSelection{Version: 1, Provider: "openai-codex", Model: "gpt-6.1-sol", Effort: "xhigh", ContextWindow: 500000}, true)
	content := strings.Repeat("このリポジトリでは、コミット前に必ずテストを実行し、結果を報告すること。\n", 64<<10/110)
	snapshot, err := projectinstructions.FromContent([]byte(content))
	if err != nil {
		t.Fatal(err)
	}
	if err = f.b.SetProjectInstructions(snapshot); err != nil {
		t.Fatal(err)
	}
	f.user(t, "task", "前回コミットのメッセージを調査して、問題があれば修正案を出して。")
	built, err := f.b.Build()
	if err != nil {
		t.Fatalf("a %d-byte AGENTS.md did not fit: %v", len(content), err)
	}
	if !strings.Contains(requestText(built), "前回コミット") {
		t.Fatal("the task is missing")
	}
}

// Codex through Docker Sandboxes lists no context window; a few large diffs
// in a turn still fit the fallback.
func TestLargeTurnFitsAnUnknownWindow(t *testing.T) {
	f := newContextFixture(t, "openai-codex", contextengine.Config{})
	f.user(t, "task", "diff:\n"+strings.Repeat("+ added line of a large diff in provider.py\n", 7000))
	if _, err := f.b.Build(); err != nil {
		t.Fatalf("a 300 KB turn did not fit an unknown window: %v", err)
	}
}

package contextbuilder

import (
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/projectinstructions"
	"github.com/unreallabsai/unreal-agent/harness/session"
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

// Earlier large tool results keep their bodies while the budget allows; shown
// only references, a model reads the same files again and again.
func TestEarlierLargeResultsKeepTheirBodies(t *testing.T) {
	f := newContextFixture(t, "openai-codex", contextengine.Config{})
	var bodies []string
	for i, name := range []string{"one", "two", "three"} {
		f.user(t, name+"-input", "read "+name)
		f.item(t, sessionstore.ItemTurn, session.Turn{ID: session.TurnID(name), Type: session.TurnRegular})
		f.item(t, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: session.TurnID(name), Response: llm.Response{Output: []llm.Item{{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: name + "-call", Name: "read", Arguments: `{"path":"source.go"}`}}}}})
		f.b.SetHistoryItem(sessionstore.Item{Sequence: f.sequence + 1, Kind: sessionstore.ItemToolCallStatus, Data: sessionstore.ToolCallStatus{TurnID: session.TurnID(name), CallID: name + "-call"}})
		body := strings.Repeat(name+" line of a file the worker read\n", 200+i)
		bodies = append(bodies, body)
		f.b.AddToolResult(name+"-call", []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: body}}, false)
		f.b.Commit()
	}
	f.user(t, "next", "now compare them")
	built, err := f.b.Build()
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range bodies {
		kept := false
		for _, item := range built.Request.Input {
			kept = kept || contextengine.Text(item) == body
		}
		if !kept {
			t.Fatalf("an earlier %d-byte result was replaced by a reference", len(body))
		}
	}
}

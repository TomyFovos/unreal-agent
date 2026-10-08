package contextbuilder

import (
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func TestTransportReservePreservesTaskAndBoundsCompactedGeneration(t *testing.T) {
	f := newContextFixture(t, "claude-code", contextengine.Config{Version: 1, InputBudget: 6000, RecentReserve: 1000, RetrievalLimit: 8})
	for n := 0; n < 500; n++ {
		id := fmt.Sprintf("t-%d", n)
		f.user(t, id, strings.Repeat("unrelated historical text ", 50))
		f.item(t, sessionstore.ItemTurn, session.Turn{ID: session.TurnID(id), Type: session.TurnRegular})
		f.item(t, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: session.TurnID(id), Response: llm.Response{Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: strings.Repeat("old answer ", 60)}}}}})
	}
	f.user(t, "current", "CURRENT_USER_TASK_MUST_SURVIVE")
	before, _ := json.Marshal(f.history)
	for _, reserve := range []int64{1200, 2400} {
		first, err := f.b.BuildWithReserve(reserve)
		if err != nil {
			t.Fatal(err)
		}
		second, err := f.b.BuildWithReserve(reserve)
		if err != nil {
			t.Fatal(err)
		}
		one, _ := json.Marshal(first.Request)
		two, _ := json.Marshal(second.Request)
		if string(one) != string(two) || !strings.Contains(string(one), "CURRENT_USER_TASK_MUST_SURVIVE") || first.Report.Context.TransportReserve != reserve || first.Report.Context.EstimatedInputTokens > 6000 || first.Package.EstimatedTokens+reserve > 6000 {
			t.Fatal("transport reservation lost task/determinism/budget")
		}
	}
	after, _ := json.Marshal(f.history)
	if string(before) != string(after) {
		t.Fatal("derived context reservation mutated canonical history")
	}
	if _, err := f.b.BuildWithReserve(6000); err == nil {
		t.Fatal("required context overflow accepted")
	}
}

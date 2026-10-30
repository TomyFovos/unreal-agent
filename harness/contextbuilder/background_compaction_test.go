package contextbuilder

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

type summaryAdapter func(llm.Request) llm.Response

func (f summaryAdapter) Respond(_ context.Context, r llm.Request, _ llm.RequestOptions) (llm.Response, error) {
	return f(r), nil
}

func TestSummaryRetainsPendingToolWithoutPinningInterveningHistory(t *testing.T) {
	f := newContextFixture(t, "openai-codex", contextengine.Config{FallbackWindow: 100000, InputBudget: 85000})
	f.user(t, "task", "Continue the feature; keep the background job running.")
	f.item(t, sessionstore.ItemTurn, session.Turn{ID: "pending", Type: session.TurnRegular})
	f.item(t, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "pending", Response: llm.Response{Output: []llm.Item{epochCall("pending", "Bash", `{"command":"long job"}`)}}})
	epochReceipt(t, f, "pending", "running", true)
	for i := range 30 {
		f.turn(t, fmt.Sprint(i), "Continue the feature.", strings.Repeat("Old explanation. ", 300))
	}
	// An update to the original live dependency reaches the very recent tail.
	epochReceipt(t, f, "pending", "still running", true)
	f.user(t, "latest", "Keep going without restarting the job.")
	var summary sessionstore.ContextRecord
	adapter := summaryAdapter(func(r llm.Request) llm.Response {
		return llm.Response{Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "Continue the feature. Background job remains running; do not restart it. Preserve the user's constraints."}}}}
	})
	err := f.b.PrepareContext(t.Context(), adapter, llm.RequestOptions{}, func(r sessionstore.ContextRecord) error {
		if r.Kind == "summary" {
			summary = r
		}
		epochApply(t, f, sessionstore.ItemHostRecord, sessionstore.HostRecord{Version: 1, Kind: sessionstore.HostContext, Context: &r})
		return nil
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Through == "" || len(summary.Retained) == 0 {
		t.Fatal("summary lost mandatory group", summary)
	}
	built, err := f.b.Build()
	if err != nil {
		t.Fatal("compacted context still exceeds budget", err)
	}
	calls, results := 0, 0
	for _, item := range built.Request.Input {
		if call, ok := item.Data.(llm.ToolCall); ok && call.CallID == "pending" {
			calls++
		}
		if result, ok := item.Data.(llm.ToolResult); ok && result.CallID == "pending" {
			results++
		}
	}
	if calls != 1 || results < 1 || strings.Count(requestText(built), "Old explanation.") >= 30*300 {
		t.Fatalf("dependency/history projection wrong: calls=%d results=%d", calls, results)
	}
	replay := newContextFixture(t, "openai-codex", f.b.contextConfig)
	for _, item := range f.history {
		epochApply(t, replay, item.Kind, item.Data)
	}
	resumed, err := replay.b.Build()
	if err != nil || !reflect.DeepEqual(built.Request, resumed.Request) {
		t.Fatal("summary replay changed the request", err)
	}
}

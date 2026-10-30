package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func TestConversationExportIncludesExecutionMeasurements(t *testing.T) {
	f := conversationFixture("public response")
	at := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	appendRecord := func(r sessionstore.HostRecord) {
		f.view.History.Items = append(f.view.History.Items, host.HistoryItem{Sequence: sessionstore.Sequence(len(f.view.History.Items) + 1), Kind: sessionstore.ItemHostRecord, Data: r})
	}
	for _, a := range []llm.ActivityRecord{{Kind: "host_start", StartedAt: at, Revision: "export-revision"}, {Kind: "child_start", StartedAt: at, Template: "worker-test", Model: "test-model", Effort: "high"}, {Kind: "native_bash", ID: "bash-1", StartedAt: at, EndedAt: at.Add(3 * time.Second)}} {
		appendRecord(sessionstore.HostRecord{Version: 1, Kind: sessionstore.HostActivity, Activity: &a})
	}
	appendRecord(sessionstore.HostRecord{Version: 1, Kind: sessionstore.HostContext, Context: &sessionstore.ContextRecord{Kind: "provider_conversation", Conversation: &llm.ConversationRecord{Provider: "claude", Phase: "compact", Compacted: true, StartedAt: at, EndedAt: at.Add(time.Minute), Usage: &llm.Usage{InputTokens: 100, OutputTokens: 10, Unknown: []llm.UsageField{llm.UsageReasoning}}}}})
	text, err := ConversationText(t.Context(), f, "s", throughHistory(f))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"export-revision", "worker-test", "test-model", "effort=high", "elapsed=3s", "Claude compact completed", "reasoning     unknown"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %s: %s", want, text)
		}
	}
}

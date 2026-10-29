package host

import (
	"context"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func TestProviderConversationRecordSurvivesHostResume(t *testing.T) {
	dir := t.TempDir()
	h := newTestHost(t, dir)
	s, err := h.Create(t.Context(), Options{ID: "conversation"})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := s.Inspect(0, 256)
	model := s.withProgress(modelFunc(func(ctx context.Context, _ llm.Request, opt llm.RequestOptions) (llm.Response, error) {
		opt.Progress(llm.Progress{Attempt: 1, Phase: "compacting"})
		view, _ := s.Inspect(0, 1)
		if view.Progress == nil || view.Progress.Mode != "compacting" || view.Progress.Text != "" {
			t.Fatal("compaction not projected", view.Progress)
		}
		record := llm.ConversationRecord{Provider: "claude", BeforeTokens: 250_000, InputTokens: 12_000, InputKnown: true, Compacted: true}
		if err := opt.RecordConversation(ctx, record); err != nil {
			return llm.Response{}, err
		}
		opt.Progress(llm.Progress{Attempt: 1, Reset: true})
		view, _ = s.Inspect(0, 1)
		if view.Progress.Mode != "streaming" {
			t.Fatal("compaction phase not cleared", view.Progress)
		}
		return llm.Response{}, nil
	}))
	if _, err = model.Respond(t.Context(), llm.Request{}, llm.RequestOptions{}); err != nil {
		t.Fatal(err)
	}
	after, _ := s.Inspect(0, 256)
	if len(after.History.Items) != len(before.History.Items)+1 {
		t.Fatal("compact changed canonical messages", after.History.Items)
	}
	item := after.History.Items[len(after.History.Items)-1]
	if record, ok := item.Data.(sessionstore.HostRecord); !ok || record.Context == nil || record.Context.Conversation == nil || !record.Context.Conversation.Compacted {
		t.Fatal("missing compact record", item)
	}
	if err = h.Close(); err != nil {
		t.Fatal(err)
	}
	h = newTestHost(t, dir)
	s, err = h.Resume(t.Context(), Options{ID: "conversation"})
	if err != nil {
		t.Fatal(err)
	}
	model = s.withProgress(modelFunc(func(_ context.Context, _ llm.Request, opt llm.RequestOptions) (llm.Response, error) {
		if opt.ConversationTokens["claude"] != 12_000 {
			t.Fatal("context not restored", opt.ConversationTokens)
		}
		return llm.Response{}, nil
	}))
	if _, err = model.Respond(t.Context(), llm.Request{}, llm.RequestOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestConversationTokensUseExplicitMeasurementOverStepTotals(t *testing.T) {
	s := progressSession()
	s.items = append(s.items, sessionstore.Item{Kind: sessionstore.ItemHostRecord, Data: sessionstore.HostRecord{Version: 1, Kind: sessionstore.HostRuntimeApplied, Selection: &sessionstore.RuntimeSelection{Provider: "claude"}}},
		sessionstore.Item{Kind: sessionstore.ItemModelResponse, Data: sessionstore.ModelResponse{Response: llm.Response{Usage: llm.Usage{InputTokens: 500_000}}}})
	if got := s.conversationTokensLocked()["claude"]; got != 500_000 {
		t.Fatal("legacy usage not available", got)
	}
	s.items = append(s.items, sessionstore.Item{Kind: sessionstore.ItemHostRecord, Data: sessionstore.HostRecord{Version: 1, Kind: sessionstore.HostContext, Context: &sessionstore.ContextRecord{Kind: "provider_conversation", Conversation: &llm.ConversationRecord{Provider: "claude", InputKnown: true, InputTokens: 12_000}}}},
		sessionstore.Item{Kind: sessionstore.ItemModelResponse, Data: sessionstore.ModelResponse{Response: llm.Response{Usage: llm.Usage{InputTokens: 999_000}}}})
	if got := s.conversationTokensLocked()["claude"]; got != 12_000 {
		t.Fatal("step totals overrode context", got)
	}
}

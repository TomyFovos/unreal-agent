package host

import (
	"context"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

type idleModel struct {
	llm.Adapter
	idle func(context.Context, llm.Request, llm.RequestOptions)
}

func (m idleModel) Idle(ctx context.Context, request llm.Request, opt llm.RequestOptions) {
	m.idle(ctx, request, opt)
}

func TestIdleProgressIsIndependentAndLeavesModelAvailable(t *testing.T) {
	s := progressSession()
	s.items = append(s.items, sessionstore.Item{Kind: sessionstore.ItemHostRecord, Data: sessionstore.HostRecord{
		Version: 1, Kind: sessionstore.HostContext, Context: &sessionstore.ContextRecord{Kind: "provider_conversation", Conversation: &llm.ConversationRecord{Provider: "claude", InputKnown: true, InputTokens: 250_000}},
	}})
	var late func(llm.Progress)
	model := s.withProgress(idleModel{
		Adapter: modelFunc(func(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
			return llm.Response{}, nil
		}),
		idle: func(ctx context.Context, _ llm.Request, opt llm.RequestOptions) {
			if opt.ConversationTokens["claude"] != 250_000 || opt.RecordConversation == nil {
				t.Fatal("idle work lost restored measurement or recorder", opt.ConversationTokens)
			}
			late = opt.Progress
			opt.Progress(llm.Progress{Attempt: 1, Phase: "compacting"})
			view, _ := s.Inspect(0, 1)
			if view.Progress == nil || view.Progress.Mode != "compacting" || view.Progress.Done || view.Progress.Text != "" {
				t.Fatal("idle compaction not projected after Respond returned", view.Progress)
			}
			if busy, _ := s.ModelBusy(); busy {
				t.Fatal("idle maintenance made the Session's model busy")
			}
		},
	})
	if _, err := model.Respond(t.Context(), llm.Request{}, llm.RequestOptions{}); err != nil {
		t.Fatal(err)
	}
	model.(llm.IdleAdapter).Idle(t.Context(), llm.Request{}, llm.RequestOptions{})
	if view, _ := s.Inspect(0, 1); view.Progress != nil {
		t.Fatal("idle phase survived maintenance completion", view.Progress)
	}
	late(llm.Progress{Attempt: 2, Phase: "compacting"})
	if view, _ := s.Inspect(0, 1); view.Progress != nil {
		t.Fatal("late maintenance callback restored the spinner", view.Progress)
	}
}

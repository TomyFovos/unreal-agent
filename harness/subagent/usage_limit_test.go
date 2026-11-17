package subagent

import (
	"context"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

type unusedUsageModel struct{}

func (unusedUsageModel) Respond(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
	panic("progress must not invoke a model")
}

func TestUsageLimitWaitNotifiesParentWithoutModelRequest(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	h, err := host.New(ctx, host.Config{Directory: t.TempDir(), Build: func(ctx context.Context, _ session.ID) (host.Runtime, error) {
		return host.Runtime{Builder: contextbuilder.NewBuilder(), LLM: unusedUsageModel{}, Tools: tool.NewRegistry(tool.StaticTranslators{}), Operations: operation.NewLocalOperationManager(ctx)}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	child, err := h.Create(ctx, host.Options{ID: "child"})
	if err != nil {
		t.Fatal(err)
	}
	inputs := make(chan inbox.Input, 4)
	ep := &endpoint{ctx: ctx, connection: "connection", local: "child", remote: "parent"}
	ep.write = func(_ context.Context, f frame) error {
		inputs <- *f.Input
		return ep.accept(frame{Version: 1, Connection: "connection", Sender: "parent", Target: "child", Kind: "ack", ID: f.ID, Receipt: &host.Receipt{ID: f.ID, Sequence: 1}})
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		forwardUsageWait(ctx, ep, child, ChildConfig{ChildID: "child", ParentID: "parent", OperationID: "start"})
	}()
	until := time.Now().Add(43 * time.Hour).UTC()
	if err := child.RecordActivity(ctx, llm.ActivityRecord{Kind: "usage_limit_wait", RetryAt: until}); err != nil {
		t.Fatal(err)
	}
	check := func(text string) {
		t.Helper()
		select {
		case in := <-inputs:
			peer, err := in.DecodePeerMessage()
			if err != nil || peer.Kind != "progress" || peer.Sender != "child" || peer.Target != "parent" || peer.Handle != "start" || peer.Text != text {
				t.Fatal(peer, err)
			}
		case <-ctx.Done():
			t.Fatal("parent did not receive usage state", ctx.Err())
		}
	}
	check(llm.UsageLimitWaiting(until))
	if err := child.RecordActivity(ctx, llm.ActivityRecord{Kind: "usage_limit_wait"}); err != nil {
		t.Fatal(err)
	}
	check("usage limit wait ended")
	cancel()
	<-done
}

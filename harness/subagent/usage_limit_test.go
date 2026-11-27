package subagent

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/responsesapi"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

type unusedUsageModel struct{}

func (unusedUsageModel) Respond(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
	panic("progress must not invoke a model")
}

type usageRetryChildModel struct {
	calls   int
	initial time.Time
	updated time.Time
}

func (m *usageRetryChildModel) Respond(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
	m.calls++
	switch m.calls {
	case 1:
		return llm.Response{}, &responsesapi.APIError{Type: "usage_limit_reached", ResetAt: m.initial}
	case 2:
		return llm.Response{}, &responsesapi.APIError{Type: "usage_limit_reached", ResetAt: m.updated}
	}
	return llm.Response{Stop: llm.StopComplete, Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "child finished"}}}}, nil
}

func TestChildUsageLimitWaitRelaysChangesAndResumes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		model := &usageRetryChildModel{initial: time.Now().Add(2 * time.Hour), updated: time.Now().Add(3 * time.Hour)}
		h, err := host.New(ctx, host.Config{Directory: t.TempDir(), Build: func(ctx context.Context, id session.ID) (host.Runtime, error) {
			var adapter llm.Adapter = unusedUsageModel{}
			if id == "child" {
				adapter = model
			}
			return host.Runtime{Builder: contextbuilder.NewBuilder(), LLM: adapter, Tools: tool.NewRegistry(tool.StaticTranslators{}), Operations: operation.NewLocalOperationManager(ctx)}, nil
		}})
		if err != nil {
			t.Fatal(err)
		}
		defer h.Close()
		parent, err := h.Create(ctx, host.Options{ID: "parent"})
		if err != nil {
			t.Fatal(err)
		}
		child, err := h.Create(ctx, host.Options{ID: "child", Lifecycle: "child", TextOnlyChild: true, Initial: []inbox.Input{
			{ID: "task", Kind: inbox.InputExternal, Payload: []byte(`"do work"`)},
		}})
		if err != nil {
			t.Fatal(err)
		}
		ep := &endpoint{ctx: ctx, connection: "connection", local: "child", remote: "parent"}
		notices := make(chan string, 4)
		ep.write = func(ctx context.Context, f frame) error {
			if f.ModelProgress != nil {
				parent.SetChildModelProgress(child.ID, *f.ModelProgress)
				return nil
			}
			receipt, err := parent.SubmitPeer(ctx, parent.Generation, string(child.ID), "start", *f.Input)
			if err != nil {
				return err
			}
			message, err := f.Input.DecodePeerMessage()
			if err != nil {
				return err
			}
			notices <- message.Text
			return ep.accept(frame{Version: 1, Connection: "connection", Sender: "parent", Target: "child", Kind: "ack", ID: f.ID, Receipt: &receipt})
		}
		go forwardUsageWait(ctx, ep, child, ChildConfig{ChildID: child.ID, ParentID: parent.ID, OperationID: "start"})
		go forwardModelProgress(ctx, ep, child, true)
		check := func(calls int, until time.Time, records int) {
			t.Helper()
			synctest.Wait()
			if got := parent.ChildModelProgress([]session.ID{child.ID}).UsageRetryAt; model.calls != calls || !got.Equal(until) {
				t.Fatalf("child calls = %d, parent's reset = %v", model.calls, got)
			}
			v, err := child.Inspect(0, 128)
			if err != nil || !v.Progress.UsageRetryAt().Equal(until) {
				t.Fatalf("child wait = %+v, %v", v.Progress, err)
			}
			count := 0
			for _, item := range v.History.Items {
				if r, ok := item.Data.(sessionstore.HostRecord); ok && r.Activity != nil && r.Activity.Kind == "usage_limit_wait" {
					count++
				}
			}
			if count != records {
				t.Fatalf("child wait has %d records, want %d", count, records)
			}
		}
		synctest.Sleep(10 * time.Millisecond)
		check(1, model.initial, 1)
		if len(notices) != 1 || <-notices != llm.UsageLimitWaiting(model.initial) {
			t.Fatal("parent did not see the child wait start")
		}
		synctest.Sleep(5 * time.Minute)
		check(2, model.updated, 1)
		if len(notices) != 0 {
			t.Fatal("the rejected retry sent an extra parent notice")
		}
		synctest.Sleep(5 * time.Minute)
		check(3, time.Time{}, 2)
		if len(notices) != 1 || <-notices != "usage limit wait ended" {
			t.Fatal("parent did not see the child resume")
		}
		if f := child.Finish(); f == nil || f.Result.Status != "completed" || f.Result.Summary != "child finished" {
			t.Fatal("child did not finish after retry", f)
		}
	})
}

func TestLegacyModelProgressOmitsUsageResets(t *testing.T) {
	until := time.Now().Add(time.Hour)
	p := host.ModelProgress{UsageRetryAt: until, Children: map[session.ID]host.ModelProgress{
		"lead": {Phase: host.ModelRequesting, UsageRetryAt: until, Children: map[session.ID]host.ModelProgress{
			"worker": {UsageRetryAt: until},
		}},
	}}
	legacy := modelPhasesOnly(p)
	lead := legacy.Children["lead"]
	if !legacy.UsageRetryAt.IsZero() || !lead.UsageRetryAt.IsZero() || len(lead.Children) != 0 || lead.Phase != host.ModelRequesting {
		t.Fatal("older parent would not receive its original phase snapshot", legacy)
	}
	if !p.Children["lead"].Children["worker"].UsageRetryAt.Equal(until) {
		t.Fatal("legacy projection mutated a shared progress snapshot")
	}
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

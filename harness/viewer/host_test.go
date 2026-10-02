package viewer

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"sync/atomic"
	"testing"
	"time"
)

type localReader struct{ h *host.Host }

func (r localReader) Inspect(ctx context.Context, id session.ID, after sessionstore.Sequence, limit int) (host.View, error) {
	if err := context.Cause(ctx); err != nil {
		return host.View{}, err
	}
	s, err := r.h.Attach(id)
	if err != nil {
		return host.View{}, err
	}
	return s.Inspect(after, limit)
}
func (r localReader) Subscribe(ctx context.Context, id session.ID, after sessionstore.Sequence, limit, capacity int) (host.Subscription, error) {
	if err := context.Cause(ctx); err != nil {
		return host.Subscription{}, err
	}
	s, err := r.h.Attach(id)
	if err != nil {
		return host.Subscription{}, err
	}
	return s.Subscribe(after, limit, capacity)
}

type viewerLLM struct{ calls atomic.Int64 }

func (m *viewerLLM) Respond(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
	n := m.calls.Add(1)
	return llm.Response{ID: fmt.Sprintf("response-%d", n), Usage: llm.Usage{InputTokens: 10, OutputTokens: 2}, Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "finished response"}}}}, nil
}
func TestRealHostProjectionPersistedHistoryRebuildWithoutLLMPolling(t *testing.T) {
	model := &viewerLLM{}
	h, err := host.New(t.Context(), host.Config{Directory: t.TempDir(), Build: func(ctx context.Context, _ session.ID) (host.Runtime, error) {
		return host.Runtime{Builder: contextbuilder.NewBuilder(), LLM: model, Tools: tool.NewRegistry(tool.StaticTranslators{}), Operations: operation.NewLocalOperationManager(ctx)}, nil
	}})
	must(t, err)
	t.Cleanup(func() { h.Close() })
	s, err := h.Create(t.Context(), host.Options{ID: "real", Lifecycle: "interactive", Policy: permission.Unrestricted()})
	must(t, err)
	c := NewClient(localReader{h}, nil, nil)
	c.PageSize = 1
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	observed := make(chan struct{}, 32)
	done := make(chan error, 1)
	go func() {
		done <- c.Watch(ctx, s.ID, func(Notice) {
			select {
			case observed <- struct{}{}:
			default:
			}
		})
	}()
	<-observed
	payload, _ := json.Marshal("do work")
	receipt, err := s.Submit(ctx, s.Generation, inbox.Input{ID: "one", Kind: inbox.InputExternal, Payload: payload})
	must(t, err)
	if receipt.Sequence == 0 {
		t.Fatal("not persisted")
	}
	for {
		d, _ := c.Model.Detail(s.ID, time.Now())
		if d.Row.Usage.Input == 10 {
			break
		}
		select {
		case <-observed:
		case <-ctx.Done():
			t.Fatal("viewer never observed response")
		}
	}
	_, err = s.Stop(ctx, s.Generation, inbox.StopWhenIdle, "done")
	must(t, err)
	must(t, s.Wait(ctx))
	must(t, <-done)
	if model.calls.Load() != 1 {
		t.Fatal("viewer or stop triggered LLM polling")
	}
	rebuilt := NewClient(localReader{h}, nil, nil)
	rebuilt.PageSize = 1
	must(t, rebuilt.Refresh(ctx, s.ID))
	got := row(t, rebuilt.Model, s.ID)
	if got.Usage.Input != 10 || got.Usage.Responses != 1 || got.Runtime != RuntimeStopped || got.Finish != nil {
		t.Fatalf("%+v", got)
	}
	page, err := rebuilt.History(ctx, s.ID, 0, 1)
	must(t, err)
	if len(page.Items) != 1 || !page.More {
		t.Fatal("bounded transcript pagination lost")
	}
	// Another viewer can attach concurrently without obtaining a writer or making
	// a second runtime. No new model response is produced by repeated reads.
	another := NewClient(localReader{h}, nil, nil)
	must(t, another.Refresh(ctx, s.ID))
	if model.calls.Load() != 1 {
		t.Fatal("read-only refresh launched execution")
	}
}

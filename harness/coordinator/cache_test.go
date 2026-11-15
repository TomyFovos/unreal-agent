package coordinator

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

type cacheTestBuilder struct {
	contextbuilder.Builder
	keys []string
}

func (b *cacheTestBuilder) PrepareContext(_ context.Context, _ llm.Adapter, opt llm.RequestOptions, _ func(sessionstore.ContextRecord) error, _ bool) error {
	b.keys = append(b.keys, opt.CacheKey)
	return nil
}

type cacheTestAdapter struct {
	llm.Adapter
	requests, idle []llm.RequestOptions
}

func (a *cacheTestAdapter) Respond(ctx context.Context, request llm.Request, opt llm.RequestOptions) (llm.Response, error) {
	a.requests = append(a.requests, opt)
	return a.Adapter.Respond(ctx, request, opt)
}

func (a *cacheTestAdapter) Idle(_ context.Context, _ llm.Request, opt llm.RequestOptions) {
	a.idle = append(a.idle, opt)
}

func TestCacheKeyReachesPreparationGenerationAndIdle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 0)
		builder := &cacheTestBuilder{Builder: run.current.dependencies.ContextBuilder}
		adapter := &cacheTestAdapter{Adapter: run.current.dependencies.LLM}
		run.current.dependencies.ContextBuilder = builder
		run.current.dependencies.LLM = adapter
		run.current.dependencies.CacheKey = "shared-prefix"
		run.start(t)
		run.input(t, externalEvent(t, 0, "first", "work"))
		run.respond(t, 0, textResponse("done"))
		if len(builder.keys) != 1 || builder.keys[0] != "shared-prefix" || len(adapter.requests) != 1 || len(adapter.idle) != 1 {
			t.Fatal("missing cache key propagation", builder.keys, adapter.requests, adapter.idle)
		}
		for _, opt := range append(adapter.requests, adapter.idle...) {
			if opt.CacheKey != "shared-prefix" || opt.Identity.SessionID != "session-1" {
				t.Fatal("cache key or conversation identity changed", opt)
			}
		}
		run.input(t, stopInput(t, "stop", inbox.StopWhenIdle))
		run.assertStopped(t)
	})
}

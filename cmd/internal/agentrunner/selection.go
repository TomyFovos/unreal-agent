package agentrunner

import (
	"context"
	"errors"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/provider"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool/observed"
	"github.com/unreallabsai/unreal-agent/internal/buildfeatures"
	"sync"
)

type selectedAdapter struct {
	mu     sync.RWMutex
	client provider.Client
}

func (a *selectedAdapter) RequiresFullToolBatch() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return llm.RequiresToolBatch(a.client)
}

func (a *selectedAdapter) Respond(ctx context.Context, r llm.Request, o llm.RequestOptions) (llm.Response, error) {
	a.mu.RLock()
	c := a.client
	a.mu.RUnlock()
	return c.Respond(ctx, r, o)
}

func (a *selectedAdapter) Idle(ctx context.Context, r llm.Request, o llm.RequestOptions) {
	a.mu.RLock()
	c := a.client
	a.mu.RUnlock()
	if idle, ok := c.(llm.IdleAdapter); ok {
		idle.Idle(ctx, r, o)
	}
}
func (a *selectedAdapter) Close() error { a.mu.RLock(); defer a.mu.RUnlock(); return a.client.Close() }
func selectionClient(c RuntimeConfig, s sessionstore.RuntimeSelection, workspace string, results *observed.Results) (provider.Client, error) {
	if s.Validate() != nil {
		return nil, errors.New("incompatible runtime selection")
	}
	if buildfeatures.Disabled(s.Provider) {
		return disabledProviderClient{}, nil
	}
	backend, err := c.ProviderRuntime(s.Provider)
	if err != nil {
		return nil, err
	}
	if s.Binding != "" && s.Binding != backend.Binding() {
		return nil, errors.New("runtime provider configuration mismatch")
	}
	selected := backend.Provider
	if s.Model != selected.Model.ID && !c.Providers.CatalogModels(s.Provider) {
		return nil, errors.New("model switching unavailable for this provider")
	}
	selected.Model.ID = s.Model
	registry, err := c.Providers.WithModel(selected)
	if err != nil {
		return nil, err
	}
	build := provider.BuildConfig{Selection: selected, Resolver: c.Credentials, HTTPClient: c.HTTPClient}
	if s.Provider == "openai-codex" && s.Fast {
		build.SupportsFast = func(ctx context.Context, model string) bool {
			catalog, err := c.Catalog(ctx, s.Provider)
			if err != nil {
				return false
			}
			found, err := catalog.FindSelection(model)
			return err == nil && found.AllowsFast(s.Provider)
		}
	}
	if c.Providers.CatalogModels(s.Provider) {
		build.RequestEffort = func(ctx context.Context, model string, selected llm.ReasoningEffort) llm.ReasoningEffort {
			if catalog, err := c.Catalog(ctx, s.Provider); err == nil {
				if found, err := catalog.FindSelection(model); err == nil {
					return found.RequestEffort(selected)
				}
			}
			return modelcatalog.Model{Efforts: []llm.ReasoningEffort{llm.ReasoningEffortMax}}.RequestEffort(selected)
		}
	}
	if backend.Claude != nil {
		build.ClaudeAgent = *backend.Claude
		build.ClaudeAgent.Directory = workspace
		build.ClaudeAgent.Results = results
		build.ClaudeAgent.Subagents = c.ClaudeSubagents
		build.ClaudeAgent.Namespace = c.SessionDirectory
		build.ClaudeAgent.CompactThresholdTokens = c.Claude.CompactThresholdTokens
	}
	client, _, err := registry.Build(build)
	return client, err
}

// disabledProviderClient preserves a saved choice without credentials or I/O.
type disabledProviderClient struct{}

func (disabledProviderClient) Close() error { return nil }
func (disabledProviderClient) Respond(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
	return llm.Response{}, buildfeatures.ErrOpenCodeGoDisabled
}

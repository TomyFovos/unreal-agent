package agentrunner

import (
	"context"
	"errors"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/provider"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"sync"
)

type selectedAdapter struct {
	mu     sync.RWMutex
	client provider.Client
}

func (a *selectedAdapter) Respond(ctx context.Context, r llm.Request, o llm.RequestOptions) (llm.Response, error) {
	a.mu.RLock()
	c := a.client
	a.mu.RUnlock()
	return c.Respond(ctx, r, o)
}
func (a *selectedAdapter) Close() error { a.mu.RLock(); defer a.mu.RUnlock(); return a.client.Close() }
func selectionClient(c RuntimeConfig, s sessionstore.RuntimeSelection) (provider.Client, error) {
	if s.Validate() != nil {
		return nil, errors.New("incompatible runtime selection")
	}
	backend, err := c.ProviderRuntime(s.Provider)
	if err != nil {
		return nil, err
	}
	if s.Binding != "" && s.Binding != backend.Binding() {
		return nil, errors.New("runtime provider configuration mismatch")
	}
	selected := backend.Provider
	if s.Model != selected.Model.ID && s.Provider != "claude-code" && s.Provider != "openai-codex" {
		return nil, errors.New("model switching unavailable for this provider")
	}
	selected.Model.ID = s.Model
	registry, err := c.Providers.WithModel(selected)
	if err != nil {
		return nil, err
	}
	build := provider.BuildConfig{Selection: selected, Resolver: c.Credentials, HTTPClient: c.HTTPClient}
	if backend.ClaudeCode != nil {
		build.ClaudeCode = *backend.ClaudeCode
		if _, observed := c.Capabilities[s.Provider]; observed {
			build.ClaudeCode.ToolBridge.Enabled = c.ProviderSupportsTools(s.Provider)
		}
		build.ClaudeCode.CatalogSource = func(ctx context.Context) (modelcatalog.Catalog, error) { return c.Catalog(ctx, s.Provider) }
		// Preserve native CLI discovery when the Host has no shared source.
		if c.Catalogs[s.Provider] == nil && c.ModelCatalog == nil {
			build.ClaudeCode.CatalogSource = nil
		}
		build.ClaudeCode.CurrentModel = llm.Model{ID: s.Model, ReasoningEffort: s.Effort}
		if c.ProviderProcess {
			build.ClaudeCode.AuthorizeProcess = func(context.Context) error { return nil }
		}
	}
	client, _, err := registry.Build(build)
	return client, err
}

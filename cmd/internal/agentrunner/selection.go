package agentrunner

import (
	"context"
	"errors"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudeagent"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/provider"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool/observed"
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
func (a *selectedAdapter) NoAutomaticReplay() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return llm.RequiresExplicitReplay(a.client)
}

func (a *selectedAdapter) ConversationRecoveryProvider() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return llm.RecoveryProvider(a.client)
}

func (a *selectedAdapter) Respond(ctx context.Context, r llm.Request, o llm.RequestOptions) (llm.Response, error) {
	a.mu.RLock()
	c := a.client
	a.mu.RUnlock()
	return c.Respond(ctx, r, o)
}
func (a *selectedAdapter) Close() error { a.mu.RLock(); defer a.mu.RUnlock(); return a.client.Close() }
func selectionClient(c RuntimeConfig, s sessionstore.RuntimeSelection, workspace string, results *observed.Results) (provider.Client, error) {
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
	if s.Model != selected.Model.ID && s.Provider != "openai-codex" && s.Provider != claudeagent.ProviderID {
		return nil, errors.New("model switching unavailable for this provider")
	}
	selected.Model.ID = s.Model
	registry, err := c.Providers.WithModel(selected)
	if err != nil {
		return nil, err
	}
	build := provider.BuildConfig{Selection: selected, Resolver: c.Credentials, HTTPClient: c.HTTPClient}
	if s.Provider == "openai-codex" {
		build.CodexRequestEffort = func(ctx context.Context, model string, selected llm.ReasoningEffort) llm.ReasoningEffort {
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
	}
	client, _, err := registry.Build(build)
	return client, err
}

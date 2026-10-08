package main

import (
	"context"
	"errors"
	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudecode"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/provider"
	"sort"
)

func registeredProviders(initial agentrunner.RuntimeIdentity, extra map[string]agentrunner.ProviderRuntime) (*provider.Registry, error) {
	catalog := map[string][]provider.Model{}
	add := func(p agentrunner.ProviderRuntime) {
		models := []provider.Model{p.Provider.Model}
		if p.ClaudeCode != nil {
			for _, m := range p.ClaudeCode.Models {
				if m.ID != p.Provider.Model.ID {
					pm := p.Provider.Model
					pm.ID = m.ID
					models = append(models, pm)
				}
			}
		}
		catalog[p.Provider.Provider] = models
	}
	add(agentrunner.ProviderRuntime{Provider: initial.Provider, ReasoningEffort: initial.ReasoningEffort, ClaudeCode: initial.ClaudeCode})
	for id, p := range extra {
		if id == initial.Provider.Provider || id != p.Provider.Provider {
			return nil, errors.New("invalid provider registration")
		}
		add(p)
	}
	return provider.New(provider.Defaults(catalog)...)
}

func (h modelUI) catalog(ctx context.Context, id string, refresh bool) (modelcatalog.Catalog, error) {
	if id == "claude-code" {
		if h.claudeRead != nil {
			return h.claudeRead(ctx, refresh)
		}
		return h.claude, nil
	}
	if id == "openai-codex" && h.cache != "" {
		return modelcatalog.ReadCodexCache(h.cache), nil
	}
	return modelcatalog.Catalog{Problem: "catalog unavailable"}, nil
}
func (h modelUI) providers(ctx context.Context) []modelcatalog.Provider {
	ids := []string{h.runtime.Identity.Provider.Provider}
	for id := range h.runtime.Backends {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var out []modelcatalog.Provider
	for _, id := range ids {
		e := h.runtime.CheckProvider(ctx, id)
		c, catalogErr := h.catalog(ctx, id, false)
		if e == nil {
			e = catalogErr
		}
		availability := "available"
		if e != nil {
			availability = "unavailable"
			var ce *claudecode.Error
			var ae *credential.Error
			if errors.As(e, &ce) && ce.Code == "external_reauth_required" || errors.As(e, &ae) && ae.Code == "external_reauth_required" {
				availability = "auth required"
			}
		}
		out = append(out, modelcatalog.Provider{ID: id, Name: modelcatalog.ProviderName(id), Availability: availability, Tools: h.runtime.ProviderSupportsTools(id), ToolBridge: h.runtime.Capabilities[id].ToolBridge, Catalog: c})
	}
	return out
}

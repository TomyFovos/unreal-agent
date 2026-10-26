package main

import (
	"context"
	"errors"
	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudeagent"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/provider"
	"sort"
)

func registeredProviders(initial agentrunner.RuntimeIdentity, extra map[string]agentrunner.ProviderRuntime) (*provider.Registry, error) {
	catalog := map[string][]provider.Model{}
	add := func(p agentrunner.ProviderRuntime) {
		models := []provider.Model{p.Provider.Model}
		catalog[p.Provider.Provider] = models
	}
	add(agentrunner.ProviderRuntime{Provider: initial.Provider, ReasoningEffort: initial.ReasoningEffort, Claude: initial.Claude})
	for id, p := range extra {
		if id == initial.Provider.Provider || id != p.Provider.Provider {
			return nil, errors.New("invalid provider registration")
		}
		add(p)
	}
	return provider.New(provider.Defaults(catalog)...)
}

func (h modelUI) catalog(ctx context.Context, id string, refresh bool) (modelcatalog.Catalog, error) {
	if id == claudeagent.ProviderID && h.claudeAgent != nil {
		return h.claudeAgent.read(ctx, refresh)
	}
	if id == "openai-codex" && h.cache != "" {
		if h.codexRead != nil {
			return h.codexRead(ctx, refresh)
		}
		return modelcatalog.ReadCodexCache(h.cache), nil
	}
	return modelcatalog.Catalog{Problem: "catalog unavailable"}, nil
}
func (h modelUI) providers(ctx context.Context) []modelcatalog.Provider {
	ids := []string{h.runtime.Identity.Provider.Provider}
	for id := range h.runtime.Backends {
		ids = append(ids, id)
	}
	for _, p := range h.supported {
		found := false
		for _, id := range ids {
			if id == p.ID {
				found = true
			}
		}
		if !found {
			ids = append(ids, p.ID)
		}
	}
	sort.Strings(ids)
	var out []modelcatalog.Provider
	for _, id := range ids {
		if _, err := h.runtime.ProviderRuntime(id); err != nil {
			for _, p := range h.supported {
				if p.ID == id {
					out = append(out, p)
				}
			}
			continue
		}
		e := h.runtime.CheckProvider(ctx, id)
		c, catalogErr := h.catalog(ctx, id, false)
		if e == nil {
			e = catalogErr
		}
		reason := modelcatalog.AvailabilityReason("")
		if e == nil && len(h.supported) > 0 && !c.Available {
			reason = modelcatalog.CatalogUnavailable
			e = errors.New("catalog unavailable")
		}
		availability := "available"
		if e != nil {
			availability = "unavailable"
			if reason == "" {
				reason = providerFailureReason(e, false)
			}
			var ca *claudeagent.Error
			var ae *credential.Error
			if errors.As(e, &ca) && ca.Code == "external_reauth_required" || errors.As(e, &ae) && ae.Code == "external_reauth_required" {
				availability = "auth required"
			}
		}
		out = append(out, modelcatalog.Provider{ID: id, Name: modelcatalog.ProviderName(id), Availability: availability, Reason: reason, Tools: h.runtime.ProviderSupportsTools(id), ToolBridge: h.runtime.Capabilities[id].ToolBridge, Catalog: c})
	}
	return out
}

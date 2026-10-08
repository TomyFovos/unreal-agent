package agentrunner

import (
	"context"
	"encoding/json/v2"
	"errors"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudecode"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/provider"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

// ProviderRuntime is operator-owned, nonsecret backend configuration. The
// creation identity stays unchanged when additional backends are registered.
// A selection of another backend binds this configuration in canonical history.
type ProviderRuntime struct {
	Provider        provider.Selection
	ReasoningEffort llm.ReasoningEffort
	ClaudeCode      *claudecode.Config `json:",omitzero"`
}

func (c RuntimeConfig) ProviderSupportsTools(id string) bool {
	if observed, ok := c.Capabilities[id]; ok {
		return observed.Tools
	}
	p, err := c.ProviderRuntime(id)
	if err != nil || c.Providers == nil {
		return false
	}
	build := provider.BuildConfig{Selection: p.Provider}
	if p.ClaudeCode != nil {
		build.ClaudeCode = *p.ClaudeCode
	}
	return c.Providers.SupportsRuntimeTools(build)
}

func (c RuntimeConfig) CheckProvider(ctx context.Context, id string) error {
	p, e := c.ProviderRuntime(id)
	if e != nil {
		return e
	}
	if check := c.Health[id]; check != nil {
		return check(ctx)
	}
	if c.Providers != nil && c.Providers.RequiresResolver(p.Provider) {
		if c.Credentials == nil {
			return &credential.Error{Code: "store_required"}
		}
		_, e = c.Credentials.Resolve(ctx, p.Provider.Auth)
	}
	return e
}

func (c RuntimeConfig) ProviderRuntime(id string) (ProviderRuntime, error) {
	if id == c.Identity.Provider.Provider {
		return ProviderRuntime{c.Identity.Provider, c.Identity.ReasoningEffort, c.Identity.ClaudeCode}, nil
	}
	p, ok := c.Backends[id]
	if !ok {
		return p, &provider.Error{Code: "provider_unregistered"}
	}
	return p, nil
}
func (p ProviderRuntime) Binding() string { b, _ := json.Marshal(p); return string(b) }

// BindSelection validates the registered backend without performing inference.
// Catalog/health authorization is performed by the Host before this is called.
func (c RuntimeConfig) BindSelection(s sessionstore.RuntimeSelection) (sessionstore.RuntimeSelection, error) {
	p, e := c.ProviderRuntime(s.Provider)
	if e != nil {
		return s, e
	}
	if s.Validate() != nil {
		return s, errors.New("invalid runtime selection")
	}
	binding := p.Binding()
	if s.Binding != "" && s.Binding != binding {
		return s, errors.New("runtime provider configuration mismatch")
	}
	s.Binding = binding
	return s, nil
}
func (c RuntimeConfig) Catalog(ctx context.Context, id string) (modelcatalog.Catalog, error) {
	if read := c.Catalogs[id]; read != nil {
		return read(ctx)
	}
	if id == c.Identity.Provider.Provider && c.ModelCatalog != nil {
		return c.ModelCatalog(ctx)
	}
	p, e := c.ProviderRuntime(id)
	if e != nil {
		return modelcatalog.Catalog{}, e
	}
	if p.ClaudeCode != nil {
		return modelcatalog.Configured(p.ClaudeCode.Models)
	}
	return modelcatalog.Catalog{Problem: "catalog unavailable"}, nil
}

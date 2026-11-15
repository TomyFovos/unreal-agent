package main

import (
	"context"
	"flag"
	"path/filepath"
	"sync"
	"time"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudeagent"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/provider"
)

func apiKeyFlags(flags *flag.FlagSet, o *launchOptions) {
	flags.StringVar(&o.openaiKeyFile, "openai-api-key-file", "", "explicit OpenAI API key file (0600); separate API billing")
	flags.StringVar(&o.anthropicKeyFile, "anthropic-api-key-file", "", "explicit Anthropic API key file (0600); separate API billing")
	flags.StringVar(&o.openaiKeyEnv, "openai-api-key-env", "", "explicit OpenAI API key environment variable name")
	flags.StringVar(&o.anthropicKeyEnv, "anthropic-api-key-env", "", "explicit Anthropic API key environment variable name")
}

func (o launchOptions) apiKeySources() (map[string]credential.Source, error) {
	out := map[string]credential.Source{}
	for id, source := range map[string]credential.Source{
		"openai":                  {File: o.openaiKeyFile, Env: o.openaiKeyEnv},
		claudeagent.APIProviderID: {File: o.anthropicKeyFile, Env: o.anthropicKeyEnv},
	} {
		if source == (credential.Source{}) {
			continue
		}
		if source.File != "" {
			path, err := filepath.Abs(source.File)
			if err != nil {
				return nil, &credential.Error{Code: "invalid_api_key_source"}
			}
			source.File = path
		}
		if err := source.Validate(); err != nil {
			return nil, err
		}
		out[id] = source
	}
	return out, nil
}

func (c serveConfiguration) apiKeySources() map[string]credential.Source {
	if c.Launcher != nil {
		return c.Launcher.APIKeys
	}
	return nil
}

func apiKeyArguments(sources map[string]credential.Source) []string {
	var args []string
	for _, entry := range []struct{ id, prefix string }{{"openai", "openai"}, {claudeagent.APIProviderID, "anthropic"}} {
		s := sources[entry.id]
		if s.File != "" {
			args = append(args, "--"+entry.prefix+"-api-key-file", s.File)
		}
		if s.Env != "" {
			args = append(args, "--"+entry.prefix+"-api-key-env", s.Env)
		}
	}
	return args
}

// API routes are registered only from explicit, nonsecret operator references.
// Neither registration nor startup discovers or consumes ambient API keys.
func registerAPIProviders(ctx context.Context, config serveConfiguration, getenv func(string) string) (serveConfiguration, error) {
	if config.Launcher == nil {
		return config, nil
	}
	for _, id := range []string{"openai", claudeagent.APIProviderID} {
		source, ok := config.Launcher.APIKeys[id]
		if !ok {
			continue
		}
		if err := source.Validate(); err != nil {
			return config, err
		}
		status := modelcatalog.Provider{ID: id, Name: modelcatalog.ProviderName(id), Availability: "available"}
		if _, err := source.Read(ctx, getenv); err != nil {
			if ctx.Err() != nil {
				return config, ctx.Err()
			}
			status = unavailableProvider(id, modelcatalog.AuthRequired)
			config.providerDiagnostics = append(config.providerDiagnostics, id+": "+err.Error())
		}
		if config.Runtime.Provider.Provider == id || config.Providers[id].Provider.Provider != "" {
			config.providerStatuses = append(config.providerStatuses, status)
			continue
		}
		p := agentrunner.ProviderRuntime{Provider: provider.Selection{Version: 1, Provider: id, Model: provider.Model{ID: leadModel}, Auth: credential.Reference{Provider: id, ID: credential.ExplicitAPIKeyID, Method: credential.APIKey}, MaxAttempts: 2, Source: "explicit API key (separate billing)"}}
		if id == "openai" {
			p.Provider.Endpoint = "https://api.openai.com/v1"
		} else {
			binary, err := claudeBinary(config, getenv)
			if err != nil {
				config.providerStatuses = append(config.providerStatuses, unavailableProvider(id, modelcatalog.CLIMissing))
				continue
			}
			p.Provider.Model.ID, p.ReasoningEffort, p.Claude = "opus", llm.ReasoningEffortHigh, &claudeagent.Config{Binary: binary}
		}
		config.Providers[id] = p
		config.providerStatuses = append(config.providerStatuses, status)
	}
	return config, nil
}

type openaiCatalog struct {
	source    credential.Source
	selection provider.Selection
	mu        sync.Mutex
	at        time.Time
	result    modelcatalog.Catalog
	err       error
}

func (c *openaiCatalog) check(ctx context.Context) error {
	_, err := c.source.Read(ctx, nil)
	return err
}

func (c *openaiCatalog) read(ctx context.Context, refresh bool) (modelcatalog.Catalog, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if refresh || c.at.IsZero() || time.Since(c.at) > time.Minute {
		key, err := c.source.Read(ctx, nil)
		if err == nil {
			c.result, err = modelcatalog.OpenAI(ctx, c.selection.Endpoint, key, nil)
		}
		c.err, c.at = err, time.Now()
	}
	return c.result.Clone(), c.err
}

func apiBackend(p agentrunner.ProviderRuntime, source credential.Source) agentrunner.DiscoveredBackend {
	b := agentrunner.DiscoveredBackend{Runtime: p}
	if p.Provider.Provider == "openai" {
		c := &openaiCatalog{selection: p.Provider, source: source}
		b.Check, b.Catalog = c.check, c.read
	} else if p.Provider.Provider == claudeagent.APIProviderID && p.Claude != nil {
		c := &claudeCatalog{binary: p.Claude.Binary, apiSource: source}
		b.Check = func(ctx context.Context) error { _, err := source.Read(ctx, nil); return err }
		b.Catalog = c.read
	}
	return b
}

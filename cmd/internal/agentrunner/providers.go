package agentrunner

import (
	"context"
	"errors"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/fireworks"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/ollama"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/openai"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/openaicodex"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/openrouter"
	"github.com/unreallabsai/unreal-agent/harness/provider"
	"net/http"
	"net/url"
)

func DefaultProviders() []Provider {
	providers := []Provider{
		{
			Name:    "ollama",
			BaseURL: ollama.BaseURL,
			NewClient: func(_, baseURL string, maxAttempts int, _ func(string) string) (Client, error) {
				return ollama.NewClient(ollama.Config{BaseURL: baseURL, MaxAttempts: &maxAttempts})
			},
		},
		{
			Name:              "openai",
			BaseURL:           "https://api.openai.com/v1",
			DefaultModel:      "gpt-6-astra",
			APIKeyEnvironment: "OPENAI_API_KEY",
			NewClient: func(apiKey, baseURL string, maxAttempts int, _ func(string) string) (Client, error) {
				return openai.NewClient(openai.Config{APIKey: apiKey, BaseURL: baseURL, MaxAttempts: &maxAttempts})
			},
		},
		{
			Name:    "openai-codex",
			BaseURL: openaicodex.BaseURL,
			NewClient: func(_, baseURL string, maxAttempts int, getenv func(string) string) (Client, error) {
				config, err := openaicodex.EnvironmentConfig(getenv)
				if err != nil {
					return nil, err
				}
				config.BaseURL, config.MaxAttempts = baseURL, &maxAttempts
				return openaicodex.NewClient(config)
			},
		},

		{
			Name:              "openrouter",
			BaseURL:           "https://openrouter.ai/api/v1",
			APIKeyEnvironment: "OPENROUTER_API_KEY",
			NewClient: func(apiKey, baseURL string, maxAttempts int, _ func(string) string) (Client, error) {
				return openrouter.NewClient(openrouter.Config{APIKey: apiKey, BaseURL: baseURL, MaxAttempts: &maxAttempts})
			},
		},
		{
			Name:              "fireworks",
			BaseURL:           "https://api.fireworks.ai/inference/v1",
			APIKeyEnvironment: "FIREWORKS_API_KEY",
			NewClient: func(apiKey, baseURL string, maxAttempts int, _ func(string) string) (Client, error) {
				return fireworks.NewClient(fireworks.Config{APIKey: apiKey, BaseURL: baseURL, MaxAttempts: &maxAttempts})
			},
		},
	}

	descriptors := provider.Defaults(nil)
	for i := range providers {
		for j := range descriptors {
			if providers[i].Name == descriptors[j].ID {
				providers[i].Descriptor = &descriptors[j]
			}
		}
	}
	return providers
}

func buildSessionClient(selected Provider, model, apiKey, endpoint string, attempts int, getenv func(string) string) (Client, provider.Selection, error) {
	if selected.Descriptor == nil {
		client, err := selected.NewClient(apiKey, endpoint, attempts, getenv)
		return client, provider.Selection{}, err
	}
	d := *selected.Descriptor
	d.Models = []provider.Model{{ID: model, Capabilities: d.Capabilities}}
	registry, err := provider.New(d)
	if err != nil {
		return nil, provider.Selection{}, err
	}
	selection := provider.Selection{Version: 1, Provider: d.ID, Model: provider.Model{ID: model}, Endpoint: endpoint, MaxAttempts: attempts, Source: "runner request/environment/configured default"}
	method := d.AuthMethods[0]
	selection.Auth = credential.Reference{Method: method}
	var resolver credential.Resolver
	if method != credential.None {
		selection.Auth.Provider = d.ID
		selection.Auth.ID = "environment"
		resolver = credential.ResolverFunc(func(context.Context, credential.Reference) (credential.Material, error) {
			return credential.Material{Token: credential.NewSecret(apiKey), Owner: credential.External}, nil
		})
	}
	if d.ID == "openai-codex" {
		config, err := openaicodex.EnvironmentConfig(getenv)
		if err != nil {
			return nil, provider.Selection{}, &credential.Error{Code: "invalid_external_source"}
		}
		selection.Auth.ID = "external-codex"
		resolver = provider.ExternalCodex(config)
	}
	if credentialID := getenv("UNREAL_HARNESS_CREDENTIAL_ID"); credentialID != "" {
		if method != credential.APIKey {
			return nil, provider.Selection{}, &credential.Error{Code: "unsupported_managed_provider"}
		}
		directory := getenv("UNREAL_HARNESS_CREDENTIAL_DIRECTORY")
		if directory == "" {
			return nil, provider.Selection{}, &credential.Error{Code: "store_required"}
		}
		store, err := credential.OpenLocal(directory)
		if err != nil {
			return nil, provider.Selection{}, err
		}
		selection.Auth.ID = credentialID
		resolver = credential.NewManager(store, nil)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Proxy configuration is scoped to this client; never set HTTPS_PROXY globally.
	if proxy := getenv("SANDBOX_EGRESS_PROXY"); proxy != "" {
		u, err := url.Parse(proxy)
		if err != nil || u.Host == "" {
			return nil, provider.Selection{}, errors.New("invalid sandbox proxy")
		}
		transport.Proxy = http.ProxyURL(u)
	}
	return registry.Build(provider.BuildConfig{Selection: selection, Resolver: resolver, HTTPClient: &http.Client{Transport: transport}})
}

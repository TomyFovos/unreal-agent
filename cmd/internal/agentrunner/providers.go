package agentrunner

import (
	"context"
	"errors"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/fireworks"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/ollama"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/openai"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/openaicodex"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/openrouter"
	"github.com/unreallabsai/unreal-agent/harness/provider"
	"net/http"
	"strings"
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

func buildSessionClient(selected Provider, model, apiKey, endpoint string, attempts int, httpClient *http.Client, getenv func(string) string) (Client, provider.Selection, error) {
	if selected.Descriptor == nil {
		client, err := selected.NewClient(apiKey, endpoint, attempts, getenv)
		return client, provider.Selection{}, err
	}
	c, err := sessionProviderConfig(selected, provider.Model{ID: model}, apiKey, endpoint, attempts, getenv)
	if err != nil {
		return nil, provider.Selection{}, err
	}
	return c.Providers.Build(provider.BuildConfig{Selection: c.Identity.Provider, Resolver: c.Credentials, HTTPClient: httpClient})
}

// sessionProviderConfig shares authentication and selection with the runner's
// RuntimeFactory path; credential material stays outside serialized identity.
func sessionProviderConfig(selected Provider, model provider.Model, apiKey, endpoint string, attempts int, getenv func(string) string) (RuntimeConfig, error) {
	d := *selected.Descriptor
	build := d.New
	d.New = func(c provider.BuildConfig, material credential.Material) (provider.Client, error) {
		client, err := build(c, material)
		if err != nil {
			return nil, err
		}
		values := []string{material.Token.Reveal()}
		if transport, ok := c.HTTPClient.Transport.(headerTransport); ok {
			for _, headers := range transport.headers {
				values = append(values, headers...)
			}
		}
		return &runnerPrivateClient{Client: client, values: values}, nil
	}
	d.Models = []provider.Model{{ID: model.ID, Family: model.Family, Capabilities: d.Capabilities}}
	registry, err := provider.New(d)
	if err != nil {
		return RuntimeConfig{}, err
	}
	selection := provider.Selection{Version: 1, Provider: d.ID, Model: model, Endpoint: endpoint, MaxAttempts: attempts, Source: "runner request/environment/configured default"}
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
			return RuntimeConfig{}, &credential.Error{Code: "invalid_external_source"}
		}
		selection.Auth.ID = "external-codex"
		resolver = provider.ExternalCodex(config)
	}
	if credentialID := getenv("UNREAL_HARNESS_CREDENTIAL_ID"); credentialID != "" {
		if method != credential.APIKey {
			return RuntimeConfig{}, &credential.Error{Code: "unsupported_managed_provider"}
		}
		directory := getenv("UNREAL_HARNESS_CREDENTIAL_DIRECTORY")
		if directory == "" {
			return RuntimeConfig{}, &credential.Error{Code: "store_required"}
		}
		store, err := credential.OpenLocal(directory)
		if err != nil {
			return RuntimeConfig{}, err
		}
		selection.Auth.ID = credentialID
		resolver = credential.NewManager(store)
	}
	return RuntimeConfig{Identity: RuntimeIdentity{Provider: selection}, Providers: registry, Credentials: resolver}, nil
}

// The shared API adapter deliberately preserves useful effort rejections. A
// gateway can echo request headers in those messages; mask known transport
// values on this runner-only route before the provider records the error.
type runnerPrivateClient struct {
	Client
	values []string
}

func (c *runnerPrivateClient) Respond(ctx context.Context, request llm.Request, options llm.RequestOptions) (llm.Response, error) {
	response, err := c.Client.Respond(ctx, request, options)
	var effort *openai.EffortError
	if errors.As(err, &effort) {
		masked := *effort
		for _, value := range c.values {
			if value == "" {
				continue
			}
			for _, field := range []*string{&masked.Model, &masked.Effort, &masked.Param, &masked.Code, &masked.Message} {
				*field = strings.ReplaceAll(*field, value, "[redacted]")
			}
		}
		err = &masked
	}
	return response, err
}

package provider

import (
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/fireworks"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/ollama"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/openai"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/openaicodex"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/openrouter"
)

// Defaults reuses the five existing clients. The caller supplies an explicit
// model catalog from its configuration/authority; this is not a stale hosted list.
func Defaults(catalog map[string][]Model) []Descriptor {
	caps := []string{"tools", "images", "reasoning"}
	return []Descriptor{
		{ID: "openai", Endpoint: "https://api.openai.com/v1", AuthMethods: []credential.Method{credential.APIKey}, Capabilities: caps, Models: catalog["openai"], New: func(c BuildConfig, m credential.Material) (Client, error) {
			return openai.NewClient(openai.Config{APIKey: m.Token.Reveal(), BaseURL: c.Selection.Endpoint, MaxAttempts: &c.Selection.MaxAttempts, HTTPClient: c.HTTPClient})
		}},
		{ID: "openrouter", Endpoint: "https://openrouter.ai/api/v1", AuthMethods: []credential.Method{credential.APIKey}, Capabilities: caps, Models: catalog["openrouter"], New: func(c BuildConfig, m credential.Material) (Client, error) {
			return openrouter.NewClient(openrouter.Config{APIKey: m.Token.Reveal(), BaseURL: c.Selection.Endpoint, MaxAttempts: &c.Selection.MaxAttempts, HTTPClient: c.HTTPClient})
		}},
		{ID: "fireworks", Endpoint: "https://api.fireworks.ai/inference/v1", AuthMethods: []credential.Method{credential.APIKey}, Capabilities: caps, Models: catalog["fireworks"], New: func(c BuildConfig, m credential.Material) (Client, error) {
			return fireworks.NewClient(fireworks.Config{APIKey: m.Token.Reveal(), BaseURL: c.Selection.Endpoint, MaxAttempts: &c.Selection.MaxAttempts, HTTPClient: c.HTTPClient})
		}},
		{ID: "ollama", Endpoint: ollama.BaseURL, AuthMethods: []credential.Method{credential.None}, Capabilities: caps, Models: catalog["ollama"], New: func(c BuildConfig, _ credential.Material) (Client, error) {
			return ollama.NewClient(ollama.Config{BaseURL: c.Selection.Endpoint, MaxAttempts: &c.Selection.MaxAttempts, HTTPClient: c.HTTPClient})
		}},
		{ID: "openai-codex", Endpoint: openaicodex.BaseURL, AuthMethods: []credential.Method{credential.OAuth}, Capabilities: caps, Models: catalog["openai-codex"], New: func(c BuildConfig, m credential.Material) (Client, error) {
			return openaicodex.NewClient(openaicodex.Config{AccessToken: m.Token.Reveal(), AccountID: m.AccountID, BaseURL: c.Selection.Endpoint, MaxAttempts: &c.Selection.MaxAttempts, HTTPClient: c.HTTPClient})
		}},
	}
}

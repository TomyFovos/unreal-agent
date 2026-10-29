package provider

import (
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudeagent"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/fireworks"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/ollama"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/openai"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/openaicodex"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/openrouter"
)

// Defaults reuses the existing clients and the installed Claude CLI, whose
// Unreal-owned tool bridge requires explicit configuration.
// The caller supplies an explicit
// model catalog from its configuration/authority; this is not a stale hosted list.
func Defaults(catalog map[string][]Model) []Descriptor {
	caps := []string{"tools", "images", "reasoning"}
	return []Descriptor{
		// Claude Code runs its own tools; Unreal approves and records them (U21).
		{ID: claudeagent.ProviderID, AuthMethods: []credential.Method{credential.OAuth}, Capabilities: []string{"reasoning"}, Models: catalog[claudeagent.ProviderID], LocalProcess: true, ExternalAuth: true, IdleMaintenance: true, ToolCapability: func(c BuildConfig) bool { return c.ClaudeAgent.Binary != "" }, New: func(c BuildConfig, _ credential.Material) (Client, error) {
			return claudeagent.NewClient(c.ClaudeAgent)
		}},
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
		{ID: "openai-codex", Endpoint: openaicodex.BaseURL, AuthMethods: []credential.Method{credential.OAuth, credential.DockerSandboxOAuth}, Capabilities: caps, Models: catalog["openai-codex"], New: func(c BuildConfig, m credential.Material) (Client, error) {
			if c.Selection.Auth.Method == credential.DockerSandboxOAuth {
				if m.Owner != credential.DockerSandboxProxy || m.Token.Reveal() != "" || m.AccountID != "" || m.RefreshToken.Reveal() != "" || !m.ExpiresAt.IsZero() || c.Selection.Endpoint != openaicodex.BaseURL {
					return nil, &credential.Error{Code: "invalid_docker_sandbox_material"}
				}
				return openaicodex.NewClient(openaicodex.Config{DockerSandboxOAuth: true, BaseURL: c.Selection.Endpoint, MaxAttempts: &c.Selection.MaxAttempts, MaxStreamAttempts: codexStreamAttempts(c.Selection), HTTPClient: c.HTTPClient, RequestEffort: c.CodexRequestEffort})
			}
			if m.Owner == credential.DockerSandboxProxy {
				return nil, &credential.Error{Code: "invalid_material"}
			}
			return openaicodex.NewClient(openaicodex.Config{AccessToken: m.Token.Reveal(), AccountID: m.AccountID, BaseURL: c.Selection.Endpoint, MaxAttempts: &c.Selection.MaxAttempts, MaxStreamAttempts: codexStreamAttempts(c.Selection), HTTPClient: c.HTTPClient, RequestEffort: c.CodexRequestEffort})
		}},
	}
}

func codexStreamAttempts(s Selection) *int {
	if s.MaxStreamAttempts == 0 {
		return nil
	}
	return &s.MaxStreamAttempts
}

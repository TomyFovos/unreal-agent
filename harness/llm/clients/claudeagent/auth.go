package claudeagent

import (
	"context"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"os"
	"strings"
)

const APIProviderID = "claude-api"

func IsProvider(id string) bool { return id == ProviderID || id == APIProviderID }

func (c *Client) providerID() string {
	if c.config.apiKey.Reveal() != "" {
		return APIProviderID
	}
	return ProviderID
}

// NewAPIClient is used only by the explicitly selected API provider. The key
// exists only in process/request configuration, never serialized configuration.
func NewAPIClient(config Config, material credential.Material) (*Client, error) {
	if material.Token.Reveal() == "" {
		return nil, &credential.Error{Code: "api_key_missing_or_invalid"}
	}
	config.apiKey = material.Token
	config.apiSource = material.Source
	return NewClient(config)
}

func authenticatedEnvironment(env []string, key credential.Secret) []string {
	env = environment(env)
	if key.Reveal() != "" {
		env = append(env, "ANTHROPIC_API_KEY="+key.Reveal())
	}
	return append(env, disableAutoCompactEnv+"=1", disableCompactEnv+"=1")
}

// DiscoverAPI uses the same initialize handshake with an explicitly selected
// source. In Claude Code 2.1.322 ANTHROPIC_API_KEY takes precedence over OAuth;
// --setting-sources does not isolate OAuth, and --bare also disables instructions.
func DiscoverAPI(ctx context.Context, binary string, environ func() []string, source credential.Source) (modelcatalog.Catalog, Account, error) {
	if environ == nil {
		environ = os.Environ
	}
	env := environ()
	key, err := source.Read(ctx, func(name string) string {
		value := ""
		for _, entry := range env {
			if k, v, _ := strings.Cut(entry, "="); k == name {
				value = v
			}
		}
		return value
	})
	if err != nil {
		return modelcatalog.Catalog{}, Account{}, err
	}
	return discover(ctx, binary, func() []string { return env }, key)
}

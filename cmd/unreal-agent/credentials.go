package main

import (
	"context"
	"path/filepath"

	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/openaicodex"
	"github.com/unreallabsai/unreal-agent/harness/provider"
)

func isExternalCodex(ref credential.Reference) bool {
	return ref == (credential.Reference{Provider: "openai-codex", Method: credential.OAuth, ID: "external-codex"})
}

func needsExternalCodex(c serveConfiguration) bool {
	uses := func(s provider.Selection) bool { return s.Provider == "openai-codex" && isExternalCodex(s.Auth) }
	if uses(c.Runtime.Provider) {
		return true
	}
	for _, p := range c.Providers {
		if uses(p.Provider) {
			return true
		}
	}
	for _, child := range c.Subagents {
		if uses(child.Runtime.Provider) {
			return true
		}
	}
	return false
}

// Discover only a file source, using the runner's official discovery semantics.
// Inline tokens/account IDs and API keys are not sources for interactive Hosts.
// Resolve the path before a child changes directory or loses the parent's HOME.
func externalCodexAuthFile(option string, getenv func(string) string) (string, error) {
	config, err := openaicodex.EnvironmentConfig(func(key string) string {
		switch key {
		case "OPENAI_CODEX_AUTH_FILE":
			if option != "" {
				return option
			}
			return getenv(key)
		case "CODEX_HOME", "HOME":
			return getenv(key)
		default:
			return ""
		}
	})
	if err != nil {
		return "", &credential.Error{Code: "invalid_external_source"}
	}
	path, err := filepath.Abs(config.AuthFile)
	if err != nil {
		return "", &credential.Error{Code: "invalid_external_source"}
	}
	return path, nil
}

// Keep the external resolver outside serializable runtime/Session identity. It
// validates and rereads the private file on each request, without taking ownership.
func withExternalCodexCredentials(managed credential.Resolver, path string) credential.Resolver {
	if path == "" {
		return managed
	}
	external := provider.ExternalCodex(openaicodex.Config{AuthFile: path})
	return credential.ResolverFunc(func(ctx context.Context, ref credential.Reference) (credential.Material, error) {
		if isExternalCodex(ref) {
			return external.Resolve(ctx, ref)
		}
		if managed == nil {
			return credential.Material{}, &credential.Error{Code: "store_required"}
		}
		return managed.Resolve(ctx, ref)
	})
}

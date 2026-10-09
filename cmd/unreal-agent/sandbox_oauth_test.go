//go:build linux || darwin

package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/openaicodex"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/session"
)

func sandboxCodexFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range map[string]string{
		"auth.json": `{"OPENAI_API_KEY":"proxy-managed"}`,
		"config.toml": `forced_login_method="api"
model_provider="sandboxd"
[model_providers.sandboxd]
name="Sandbox Proxy"
base_url="https://chatgpt.com/backend-api/codex"
experimental_bearer_token="oai-oat01-proxy-managed"
requires_openai_auth=false
`,
		"models_cache.json": normalCodexCache,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "auth.json")
}

func TestDockerSandboxCodexDiscoveryIsExplicit(t *testing.T) {
	path := sandboxCodexFixture(t)
	getenv := func(key string) string {
		if key == "HTTPS_PROXY" {
			return "http://host.docker.internal:3128"
		}
		if key == "SBX_CRED_OPENAI_MODE" {
			return "oauth"
		}
		return ""
	}
	if _, err := discoverCodex(t.Context(), path, getenv); err == nil {
		t.Fatal("normal discovery inferred proxy authentication")
	}
	runtime, err := discoverCodexSource(t.Context(), path, getenv, true)
	if err != nil || !isDockerSandboxCodex(runtime.Provider.Auth) || runtime.Provider.Endpoint != openaicodex.BaseURL || runtime.Provider.Model.ID != "gpt-6.1-sol" {
		t.Fatal("explicit discovery failed", err)
	}
	if _, err = discoverCodexSource(t.Context(), path, func(string) string { return "apikey" }, true); !credential.IsCode(err, "docker_sandbox_oauth_unavailable") {
		t.Fatal("API-key kit was accepted", err)
	}
	t.Setenv("SBX_CRED_OPENAI_MODE", "oauth")
	resolver := withExternalCodexCredentials(nil, path)
	if _, err = resolver.Resolve(t.Context(), runtime.Provider.Auth); err != nil {
		t.Fatal(err)
	}
	if _, err = resolver.Resolve(t.Context(), codexReference()); !credential.IsCode(err, "external_reauth_required") {
		t.Fatal("proxy/native source confusion", err)
	}
	if !slices.Contains(childEnvironment(agentrunner.RuntimeConfig{Identity: agentrunner.RuntimeIdentity{Provider: runtime.Provider}}), "SBX_CRED_OPENAI_MODE=oauth") {
		t.Fatal("missing closed child mode")
	}
}

func TestDockerSandboxCodexHostAndCatalogNoInference(t *testing.T) {
	t.Setenv("SBX_CRED_OPENAI_MODE", "oauth")
	path := sandboxCodexFixture(t)
	cfg := codexServeConfiguration(t.TempDir(), openaicodex.BaseURL)
	cfg.Runtime.Provider.Auth = credential.Reference{Provider: "openai-codex", ID: "docker-sandbox-oauth", Method: credential.DockerSandboxOAuth}
	discover := false
	cfg.Launcher = &normalLauncherConfig{Version: 1, DiscoverProviders: &discover}
	cfg.Permissions = permission.Config{Tools: []string{"read", "grep", "glob"}, ReadRoots: []string{cfg.Runtime.Workspace}, NetworkOrigins: []string{"https://chatgpt.com"}}
	client, _, output := startInteractiveHost(t, cfg, "--codex-auth-file", path)
	view, err := client.Open(t.Context(), host.Create, session.ID("sandbox-no-inference"))
	if err != nil || !view.Running || len(view.Operations) != 0 || responseCount(view) != 0 {
		t.Fatal("idle Host created work", err)
	}
	catalog, err := modelExchange(t.Context(), client.Extension, modelRequest{Action: "model.catalog", ID: "sandbox-no-inference"})
	if err != nil || catalog.Catalog == nil || !catalog.Catalog.Available || len(catalog.Catalog.Models) != 1 {
		t.Fatal("/model catalog unavailable", err)
	}
	if strings.Contains(output.String(), "proxy-managed") || strings.Contains(output.String(), "oai-oat") {
		t.Fatal("proxy placeholders leaked to diagnostics")
	}
}

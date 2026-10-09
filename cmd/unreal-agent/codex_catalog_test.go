//go:build linux || darwin

package main

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/openaicodex"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/internal/testcodex"
)

const proxyCatalogPage = `{"data":[{"id":"model-a","model":"model-a","displayName":"Model A","hidden":false,"defaultReasoningEffort":"medium","supportedReasoningEfforts":[{"reasoningEffort":"low"},{"reasoningEffort":"medium"}]},{"id":"model-b","model":"model-b","displayName":"Model B","hidden":false,"defaultReasoningEffort":"high","supportedReasoningEfforts":[{"reasoningEffort":"high"},{"reasoningEffort":"xhigh"}]}],"nextCursor":null}`

func proxyCatalogCLI(t *testing.T, home string, failed bool) {
	t.Helper()
	var reply any
	if failed {
		reply = map[string]any{"id": 2, "error": map[string]any{"code": -1, "message": "private-error-body"}}
	} else {
		var value any
		if err := json.Unmarshal([]byte(proxyCatalogPage), &value); err != nil {
			t.Fatal(err)
		}
		reply = map[string]any{"id": 2, "result": value}
	}
	binary := testcodex.Install(t, home, []any{map[string]any{"id": 1, "result": map[string]any{"userAgent": "synthetic"}}, reply})
	t.Setenv("HOME", home)
	t.Setenv("PATH", filepath.Dir(binary)+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestDockerSandboxCatalogMissingCacheMetadataAndSelectionNoInference(t *testing.T) {
	t.Setenv("SBX_CRED_OPENAI_MODE", "oauth")
	path := sandboxCodexFixture(t)
	if err := os.Remove(codexModelCache(path)); err != nil {
		t.Fatal(err)
	}
	proxyCatalogCLI(t, t.TempDir(), false)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	cfg := codexServeConfiguration(t.TempDir(), openaicodex.BaseURL)
	cfg.Runtime.Provider.Auth = credential.Reference{Provider: "openai-codex", ID: "docker-sandbox-oauth", Method: credential.DockerSandboxOAuth}
	discover := false
	cfg.Launcher = &normalLauncherConfig{Version: 1, DiscoverProviders: &discover}
	cfg.Permissions = permission.Config{Tools: []string{"read", "grep", "glob"}, ReadRoots: []string{cfg.Runtime.Workspace}, NetworkOrigins: []string{"https://chatgpt.com"}}
	client, _, _ := startInteractiveHost(t, cfg, "--codex-auth-file", path)
	view, err := client.Open(t.Context(), host.Create, session.ID("model-list-only"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := modelExchange(t.Context(), client.Extension, modelRequest{Action: "model.catalog", ID: "model-list-only"})
	if err != nil || result.Catalog == nil || !result.Catalog.Available || len(result.Catalog.Models) != 2 {
		t.Fatal("no catalog without cache", err)
	}
	choice := sessionstore.RuntimeSelection{Version: 1, RequestID: "model-list-choice", Provider: "openai-codex", Model: "model-b", Effort: "xhigh"}
	if _, err = modelExchange(t.Context(), client.Extension, modelRequest{Action: "model.select", ID: "model-list-only", Generation: view.Generation, Selection: choice}); err != nil {
		t.Fatal(err)
	}
	choice.RequestID, choice.Effort = "invalid-effort", "medium"
	if _, err = modelExchange(t.Context(), client.Extension, modelRequest{Action: "model.select", ID: "model-list-only", Generation: view.Generation, Expected: 1, Selection: choice}); err == nil {
		t.Fatal("model-specific effort bypassed")
	}
	after, err := client.Inspect(t.Context(), "model-list-only", 0, 256)
	if err != nil || len(after.Operations) != 0 || responseCount(after) != 0 {
		t.Fatal("picker started inference or Operation", err)
	}
	data, _ := os.ReadFile(path)
	current, _ := os.Stat(path)
	if string(data) != string(before) || !current.ModTime().Equal(info.ModTime()) {
		t.Fatal("auth changed")
	}
	requests, _ := os.ReadFile(filepath.Join(os.Getenv("HOME"), "codex-requests.jsonl"))
	if strings.Contains(string(requests), "thread/") || strings.Contains(string(requests), "turn/") || strings.Contains(string(requests), "proxy-managed") {
		t.Fatal("execution/credential request emitted")
	}
}

func TestDockerSandboxCatalogFailureKeepsCurrentAndNativeNeverStartsCLI(t *testing.T) {
	t.Setenv("SBX_CRED_OPENAI_MODE", "oauth")
	path := sandboxCodexFixture(t)
	if err := os.Remove(codexModelCache(path)); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	proxyCatalogCLI(t, home, true)
	native, _ := codexCatalogReader(path, false, os.Getenv)(t.Context(), false)
	if native.Available || native.ProblemCode != "codex_cache_missing" {
		t.Fatal("native contract changed")
	}
	if _, err := os.Stat(filepath.Join(home, "codex-starts.jsonl")); !os.IsNotExist(err) {
		t.Fatal("native launched Codex")
	}
	c, err := codexCatalogReader(path, true, os.Getenv)(t.Context(), false)
	if err != nil || c.Available || len(c.Models) != 0 || c.ProblemCode != "codex_cli_rejected" || strings.Contains(c.Problem, "private") {
		t.Fatal("wrong catalog denial", err)
	}
	// Explicit discovery gets the same metadata and never changes auth identity.
	proxyCatalogCLI(t, home, false)
	p, err := discoverCodexSource(t.Context(), path, os.Getenv, true)
	if err != nil || !isDockerSandboxCodex(p.Provider.Auth) || p.Provider.Model.ID != "model-a" || p.ReasoningEffort != "medium" {
		t.Fatal("discovery did not share catalog", err)
	}
}

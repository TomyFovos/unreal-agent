//go:build linux || darwin

package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudeagent"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/provider"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

func apiFixture(t *testing.T, endpoint string) (agentrunner.ProviderRuntime, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "api.key")
	if err := os.WriteFile(path, []byte("fake-selected-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	return agentrunner.ProviderRuntime{Provider: provider.Selection{Version: 1, Provider: "openai", Model: provider.Model{ID: leadModel}, Endpoint: endpoint, Auth: credential.Reference{Provider: "openai", ID: "explicit-api-key", Method: credential.APIKey}, MaxAttempts: 1, Source: "explicit API key (separate billing)"}}, path
}

func TestAPIKeyBootstrapRequiresExplicitRegistrationAndSelection(t *testing.T) {
	home := t.TempDir()
	getenv := func(k string) string {
		if k == "HOME" {
			return home
		}
		if k == "OPENAI_API_KEY" || k == "ANTHROPIC_API_KEY" {
			return "ambient-fake-key"
		}
		return ""
	}
	o := launchOptions{bootstrapCodexDisabled: true, bootstrapClaudeDisabled: true}
	cfg, err := bootstrapRuntime(t.Context(), o, chooseInitialProvider(io.Discard), getenv)
	if err != nil || cfg.Runtime.Provider.Provider != loginProvider || len(cfg.Providers) != 0 {
		t.Fatal("ambient API key registered a route")
	}
	_, key := apiFixture(t, "https://unused.invalid")
	o.openaiKeyFile = key
	cfg, err = bootstrapRuntime(t.Context(), o, chooseInitialProvider(io.Discard), getenv)
	if err != nil || cfg.Runtime.Provider.Provider != loginProvider || cfg.apiKeySources()["openai"].File != key {
		t.Fatal("API registration changed initial selection")
	}
	selected := func(k string) string {
		if k == "UNREAL_HARNESS_LLM_PROVIDER" {
			return "openai"
		}
		return getenv(k)
	}
	cfg, err = bootstrapRuntime(t.Context(), o, chooseInitialProvider(io.Discard), selected)
	if err != nil || cfg.Runtime.Provider.Provider != "openai" {
		t.Fatal("explicit initial API choice failed")
	}
	if err := os.Chmod(key, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err = bootstrapRuntime(t.Context(), o, chooseInitialProvider(io.Discard), getenv)
	if err != nil {
		t.Fatal("unselected API source stopped normal startup")
	}
	if err := apiBackend(cfg.Providers["openai"], cfg.apiKeySources()["openai"]).Check(t.Context()); !credential.IsCode(err, "api_key_file_requires_0600") {
		t.Fatal("insecure API source accepted as usable")
	}
}

func TestAPIKeyModelSelectionUsageResumeAndNoFallback(t *testing.T) {
	var apiCalls, subCalls, modelCalls atomic.Int32
	var expected atomic.Value
	expected.Store("fake-selected-key")
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+expected.Load().(string) || r.Header.Get("ChatGPT-Account-ID") != "" {
			t.Error("API request used wrong authentication")
		}
		if r.URL.Path == "/models" {
			modelCalls.Add(1)
			io.WriteString(w, `{"data":[{"id":"gpt-api-a"},{"id":"gpt-api-b"}]}`)
			return
		}
		if r.URL.Path != "/responses" {
			t.Error("unexpected API endpoint")
		}
		apiCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"API answer\"}]}],\"usage\":{\"input_tokens\":17,\"output_tokens\":9}}}\n\n")
	}))
	defer api.Close()
	sub := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		subCalls.Add(1)
		t.Error("subscription inference unexpectedly called")
	}))
	defer sub.Close()
	workspace := privateCLIDirectory(t)
	auth := filepath.Join(workspace, "auth.json")
	writeCodexAuth(t, auth, "fake-sub-token", "fake-sub-account")
	cache := `{"models":[{"slug":"gpt-6.1-sol","visibility":"list","supported_reasoning_levels":[{"effort":"low"}],"default_reasoning_level":"low"}]}`
	if err := os.WriteFile(filepath.Join(workspace, "models_cache.json"), []byte(cache), 0o600); err != nil {
		t.Fatal(err)
	}
	p, key := apiFixture(t, api.URL)
	cfg := codexServeConfiguration(workspace, sub.URL)
	cfg.Providers = map[string]agentrunner.ProviderRuntime{"openai": p}
	no := false
	cfg.Launcher = &normalLauncherConfig{Version: 1, DiscoverProviders: &no, APIKeys: map[string]credential.Source{"openai": {File: key}}}
	cfg.Permissions.NetworkOrigins = append(cfg.Permissions.NetworkOrigins, api.URL)
	c, store, out := startInteractiveHost(t, cfg, "--codex-auth-file", auth, "--normal-runtime")
	v, err := c.Open(t.Context(), host.Create, "api-selection")
	if err != nil {
		t.Fatal(err)
	}
	providers, err := modelExchange(t.Context(), c.Extension, modelRequest{Action: "model.providers", ID: v.Session.Session.ID})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range providers.Providers {
		if p.ID == "openai" {
			found = strings.Contains(p.Name, "API キー") && strings.Contains(p.Name, "別課金") && p.Catalog.Available
		}
	}
	if !found || apiCalls.Load() != 0 || modelCalls.Load() != 0 {
		t.Fatal("API route was not separately listed without inference")
	}
	choice := sessionstore.RuntimeSelection{Version: 1, Provider: "openai", Model: "gpt-api-b", RequestID: "select-api"}
	if _, err := modelExchange(t.Context(), c.Extension, modelRequest{Action: "model.select", ID: v.Session.Session.ID, Generation: v.Generation, Selection: choice}); err != nil {
		t.Fatal(err)
	}
	submitInteractive(t, c, v, "api-1", "hello")
	v = inspectInteractive(t, c, v.Session.Session.ID, func(v host.View) bool { return responseCount(v) == 1 && v.Progress == nil })
	if multiReport(t, v).Selection.Provider != "openai" || apiCalls.Load() != 1 || subCalls.Load() != 0 {
		t.Fatal("API choice did not remain on the selected connection")
	}
	usageFound := false
	for _, item := range v.History.Items {
		if response, ok := item.Data.(sessionstore.ModelResponse); ok {
			usageFound = response.Response.Usage.AuthMethod == "api_key" && response.Response.Usage.InputTokens == 17 && response.Response.Usage.OutputTokens == 9
		}
	}
	if !usageFound {
		t.Fatal("API usage or authentication method not recorded")
	}
	stopMultiSession(t, c, v)
	expected.Store("fake-rotated-key")
	// The operator may move the key file without changing a Session's API
	// connection identity or invalidating its recorded provider binding.
	moved := filepath.Join(t.TempDir(), "moved-api.key")
	if err := os.Rename(key, moved); err != nil {
		t.Fatal(err)
	}
	key = moved
	cfg.Launcher.APIKeys["openai"] = credential.Source{File: key}
	if err := os.WriteFile(key, []byte("fake-rotated-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	c2, _, out2 := startInteractiveHost(t, cfg, "--codex-auth-file", auth, "--session-directory", store, "--normal-runtime")
	v, err = c2.Open(t.Context(), host.Resume, "api-selection")
	if err != nil {
		t.Fatal(err)
	}
	submitInteractive(t, c2, v, "api-2", "hello again")
	v = inspectInteractive(t, c2, "api-selection", func(v host.View) bool { return responseCount(v) == 2 && v.Progress == nil })
	if apiCalls.Load() != 2 || subCalls.Load() != 0 || multiReport(t, v).Selection.Provider != "openai" {
		t.Fatal("resume changed API selection or used stale key")
	}
	if err := os.Remove(key); err != nil {
		t.Fatal(err)
	}
	submitInteractive(t, c2, v, "api-missing", "hello")
	v = inspectInteractive(t, c2, "api-selection", func(v host.View) bool { return hasFinalText(v, "API キー") && v.Progress == nil })
	if apiCalls.Load() != 2 || subCalls.Load() != 0 || multiReport(t, v).Selection.Provider != "openai" {
		t.Fatal("missing API key caused fallback")
	}
	// A subscription Session must retain its login guidance despite the API route.
	v2, err := c2.Open(t.Context(), host.Create, "subscription-expired")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(auth); err != nil {
		t.Fatal(err)
	}
	submitInteractive(t, c2, v2, "sub-missing", "hello")
	v2 = inspectInteractive(t, c2, "subscription-expired", func(v host.View) bool { return hasFinalText(v, "codex login") && v.Progress == nil })
	if apiCalls.Load() != 2 || multiReport(t, v2).Selection.Provider != "openai-codex" {
		t.Fatal("expired subscription caused API fallback")
	}
	stopMultiSession(t, c2, v)
	stopMultiSession(t, c2, v2)
	assertNoStoredCredentials(t, store, "fake-selected-key", "fake-rotated-key", "fake-sub-token", "fake-sub-account")
	assertNoCredentialLeak(t, out.String()+out2.String(), "fake-selected-key", "fake-rotated-key")
}

func TestAPIKeyBuiltInChildrenFollowSelectedConnection(t *testing.T) {
	workspace := t.TempDir()
	p, key := apiFixture(t, "https://unused.invalid")
	cfg := codexServeConfiguration(workspace, "https://unused.invalid")
	cfg.Providers = map[string]agentrunner.ProviderRuntime{"openai": p}
	maximum := permission.Config{Tools: tool.StaticNames(), FilesystemUnrestricted: true, NetworkUnrestricted: true, ProcessMode: permission.ProcessUnrestricted}
	cfg.PermissionModes = &permission.ModesConfig{Enabled: true, Maximum: &maximum}
	registry, err := registeredProviders(cfg.Runtime, cfg.Providers)
	if err != nil {
		t.Fatal(err)
	}
	runtime := agentrunner.RuntimeConfig{BuiltinSubagents: true, Identity: cfg.Runtime, Backends: cfg.Providers, Providers: registry, SessionDirectory: t.TempDir(), Credentials: credential.WithSources(nil, map[string]credential.Source{"openai": {File: key}}, nil)}
	_, templates, err := withParentSubagents(runtime, defaultSubagents(cfg, subagentModel{}), permission.Unrestricted(), "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"openai", "openai-codex"} {
		converted, err := builtinGPTTemplate(runtime, templates["lead"], id)
		if err != nil {
			t.Fatal(err)
		}
		selection := sessionstore.SelectionFromConfiguration(converted.Runtime)
		if selection == nil || selection.Provider != id || converted.Default.Provider != id {
			t.Fatal("lead connection mismatch")
		}
		for _, worker := range converted.Subagents {
			if sessionstore.SelectionFromConfiguration(worker.Runtime).Provider != id {
				t.Fatal("grandchild changed connection")
			}
		}
		if selection.Effort != llm.ReasoningEffortMax || converted.Default.Effort != llm.ReasoningEffortMax {
			t.Fatal("invalid effort metadata policy")
		}
		if childTemplateValidator(runtime)(templates["lead"], converted) != nil {
			t.Fatal("recorded child failed recovery validation")
		}
	}
	if sessionstore.SelectionFromConfiguration(templates["lead"].Runtime).Provider != "openai-codex" {
		t.Fatal("Session choice mutated shared templates")
	}
	if modelcatalog.ProviderName(claudeagent.APIProviderID) == modelcatalog.ProviderName(claudeagent.ProviderID) {
		t.Fatal("Claude API and subscription labels are identical")
	}
}

func TestAPIKeyClaudeSelectionAndUsage(t *testing.T) {
	_, cli := normalFixture(t, false, true)
	cli.Set(t, testclaude.Config{Catalog: hostClaudeCatalog, AgentFinal: "Claude selected connection", AgentInputTokens: 100})
	t.Setenv("ANTHROPIC_API_KEY", "ambient-fake-key")
	p, key := apiFixture(t, "")
	p.Provider.Provider, p.Provider.Auth.Provider = claudeagent.APIProviderID, claudeagent.APIProviderID
	p.Provider.Model.ID, p.ReasoningEffort, p.Claude = "opus", llm.ReasoningEffortHigh, &claudeagent.Config{Binary: cli.Binary}
	o := launchOptions{bootstrapClaude: cli.Binary}
	cfg, err := bootstrapRuntime(t.Context(), o, chooseInitialProvider(io.Discard), os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Providers[claudeagent.APIProviderID] = p
	cfg.Launcher.APIKeys = map[string]credential.Source{claudeagent.APIProviderID: {File: key}}
	c, store, out := startInteractiveHost(t, cfg)
	v, err := c.Open(t.Context(), host.Create, "claude-api")
	if err != nil {
		t.Fatal(err)
	}
	providers, err := modelExchange(t.Context(), c.Extension, modelRequest{Action: "model.providers", ID: v.Session.Session.ID})
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	for _, p := range providers.Providers {
		names[p.ID] = p.Name
	}
	if names[claudeagent.APIProviderID] == "" || names[claudeagent.APIProviderID] == names[claudeagent.ProviderID] {
		t.Fatal("Claude connections not distinguished")
	}
	submitInteractive(t, c, v, "subscription", "hello")
	v = inspectInteractive(t, c, "claude-api", func(v host.View) bool { return responseCount(v) == 1 && v.Progress == nil })
	for _, call := range cli.Calls(t) {
		for _, entry := range call.Environment {
			if strings.HasPrefix(entry, "ANTHROPIC_API_KEY=") {
				t.Fatal("unselected API key reached subscription process")
			}
		}
	}
	catalog, err := modelExchange(t.Context(), c.Extension, modelRequest{Action: "model.catalog", ID: v.Session.Session.ID, Provider: claudeagent.APIProviderID})
	if err != nil || catalog.Catalog == nil || !catalog.Catalog.Available {
		t.Fatal("Claude API catalog unavailable", err)
	}
	choice := sessionstore.RuntimeSelection{Version: 1, Provider: claudeagent.APIProviderID, Model: "discovered-b", Effort: llm.ReasoningEffortHigh, RequestID: "choose-claude-api"}
	if _, err := modelExchange(t.Context(), c.Extension, modelRequest{Action: "model.select", ID: v.Session.Session.ID, Generation: v.Generation, Selection: choice}); err != nil {
		t.Fatal(err)
	}
	submitInteractive(t, c, v, "api", "hello")
	v = inspectInteractive(t, c, "claude-api", func(v host.View) bool { return responseCount(v) == 2 && v.Progress == nil })
	usage, activity := false, false
	for _, item := range v.History.Items {
		if r, ok := item.Data.(sessionstore.ModelResponse); ok && r.Response.Usage.AuthMethod == "api_key" {
			usage = r.Response.Usage.InputTokens > 0 && r.Response.Usage.OutputTokens == 40
		}
		if r, ok := item.Data.(sessionstore.HostRecord); ok && r.Activity != nil && r.Activity.Kind == "claude_usage" && r.Activity.Provider == claudeagent.APIProviderID {
			activity = r.Activity.Usage != nil && r.Activity.Usage.AuthMethod == "api_key" && r.Activity.Usage.OutputTokens == 40
		}
	}
	if !usage || !activity {
		t.Fatal("Claude API token counts or connection metadata missing")
	}
	calls := cli.Calls(t)
	last := calls[len(calls)-1]
	apiEnv := false
	for _, entry := range last.Environment {
		apiEnv = apiEnv || entry == "ANTHROPIC_API_KEY=[redacted]"
	}
	if !apiEnv {
		t.Fatal("explicit Claude API key was not injected")
	}
	if err := os.Remove(key); err != nil {
		t.Fatal(err)
	}
	submitInteractive(t, c, v, "missing", "hello")
	v = inspectInteractive(t, c, "claude-api", func(v host.View) bool { return hasFinalText(v, "API キー") && v.Progress == nil })
	if len(cli.Calls(t)) != len(calls) || multiReport(t, v).Selection.Provider != claudeagent.APIProviderID {
		t.Fatal("missing Claude API key fell back to subscription")
	}
	stopMultiSession(t, c, v)
	assertNoStoredCredentials(t, store, "fake-selected-key", "ambient-fake-key")
	assertNoStoredCredentials(t, cli.Directory, "fake-selected-key", "ambient-fake-key")
	assertNoCredentialLeak(t, out.String(), "fake-selected-key", "ambient-fake-key")
}

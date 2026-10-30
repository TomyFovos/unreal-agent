package agentrunner

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/dap"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/lsp"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/profile"
	"github.com/unreallabsai/unreal-agent/harness/provider"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

func TestLegacyUltraRuntimeAndSessionResumeUseMax(t *testing.T) {
	var sent string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Reasoning struct {
				Effort string `json:"effort"`
			} `json:"reasoning"`
		}
		if err := json.UnmarshalRead(r.Body, &body); err != nil {
			t.Error(err)
		}
		sent = body.Reasoning.Effort
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[]}}\n\n")
	}))
	defer server.Close()
	model := provider.Model{ID: "gpt-6.1-sol"}
	registry, err := provider.New(provider.Defaults(map[string][]provider.Model{"openai-codex": {model}})...)
	if err != nil {
		t.Fatal(err)
	}
	identity := RuntimeIdentity{Version: 1, Workspace: t.TempDir(), Profile: profile.Default(), ReasoningEffort: llm.ReasoningEffortUltra,
		Provider: provider.Selection{Version: 1, Provider: "openai-codex", Model: model, Endpoint: server.URL, Auth: credential.Reference{Provider: "openai-codex", Method: credential.OAuth, ID: "synthetic"}, MaxAttempts: 1, Source: "test"}}
	directory := t.TempDir()
	factory, raw, err := NewRuntimeFactory(RuntimeConfig{Identity: identity, SessionDirectory: directory, Providers: registry,
		Credentials: credential.ResolverFunc(func(context.Context, credential.Reference) (credential.Material, error) {
			return credential.Material{Token: credential.NewSecret("synthetic-token"), AccountID: "synthetic-account"}, nil
		}),
		ModelCatalog: func(context.Context) (modelcatalog.Catalog, error) {
			return modelcatalog.Catalog{Available: true, Models: []modelcatalog.Model{{ID: model.ID, Efforts: []llm.ReasoningEffort{"high", "xhigh", "max"}, MultiAgentEffort: "xhigh"}}}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	var recorded RuntimeIdentity
	if err := json.Unmarshal(raw, &recorded); err != nil || recorded.ReasoningEffort != llm.ReasoningEffortUltra {
		t.Fatal("immutable creation identity changed", recorded.ReasoningEffort, err)
	}
	ctx, cancel := context.WithCancel(permission.WithPolicy(t.Context(), permission.Unrestricted()))
	defer cancel()
	runtime, err := factory(ctx, "legacy-request")
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	built, err := runtime.Builder.Build()
	if err != nil || built.Request.Model.ReasoningEffort != llm.ReasoningEffortMax {
		t.Fatal("legacy runtime did not build max", err)
	}
	if _, err := runtime.LLM.Respond(ctx, built.Request, llm.RequestOptions{}); err != nil || sent != "max" {
		t.Fatal("legacy runtime sent effort", sent, err)
	}
	for _, resume := range []bool{false, true} {
		owner, err := host.New(t.Context(), host.Config{Directory: directory, Build: factory})
		if err != nil {
			t.Fatal(err)
		}
		options := host.Options{ID: "legacy-session", Configuration: raw}
		var s *host.Session
		if resume {
			s, err = owner.Resume(t.Context(), options)
		} else {
			s, err = owner.Create(t.Context(), options)
		}
		if err != nil {
			owner.Close()
			t.Fatal(err)
		}
		selection, _ := s.RuntimeSelection()
		if selection == nil || selection.Effort != llm.ReasoningEffortMax {
			owner.Close()
			t.Fatal("legacy Session selection", selection)
		}
		if err := owner.Close(); err != nil {
			t.Fatal(err)
		}
		<-s.Done()
	}
}

func TestRuntimeFactoryOwnsFreshSessionComponentsAndIdentity(t *testing.T) {
	catalog := map[string][]provider.Model{"ollama": {{ID: "explicit-model", Family: "custom"}}}
	registry, err := provider.New(provider.Defaults(catalog)...)
	if err != nil {
		t.Fatal(err)
	}
	identity := RuntimeIdentity{Version: 1, Workspace: t.TempDir(), SystemPrompt: "explicit system", Provider: provider.Selection{Version: 1, Provider: "ollama", Model: catalog["ollama"][0], Auth: credential.Reference{Method: credential.None}, Source: "test", MaxAttempts: 1}, Profile: profile.Default(), ReasoningEffort: llm.ReasoningEffort("low"), DisallowedTools: []string{tool.BashName}}
	languages, err := NewLanguageTools(permission.WithPolicy(t.Context(), permission.Unrestricted()), identity.Workspace, []lsp.ServerConfig{{Language: "go", Path: "/missing/gopls"}})
	if err != nil {
		t.Fatal(err)
	}
	defer languages.Close()
	c := RuntimeConfig{Identity: identity, SessionDirectory: filepath.Join(t.TempDir(), "sessions"), Providers: registry, LanguageTools: languages, DebugAdapters: []dap.AdapterConfig{{ID: "debug", Path: "/missing/adapter", Directory: identity.Workspace}}}
	factory, raw, err := NewRuntimeFactory(c)
	if err != nil {
		t.Fatal(err)
	}
	identity.DisallowedTools[0] = tool.ReadName
	var saved RuntimeIdentity
	if err = json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.LanguageServers != languages.Identity() || len(saved.DebugAdapters) != 64 {
		t.Fatal("protocol configuration identity missing", saved)
	}
	if saved.DisallowedTools[0] != tool.BashName {
		t.Fatal("identity alias")
	}
	ctx, cancel := context.WithCancel(permission.WithPolicy(t.Context(), permission.Unrestricted()))
	first, err := factory(ctx, "one")
	if err != nil {
		t.Fatal(err)
	}
	second, err := factory(ctx, "two")
	if err != nil {
		t.Fatal(err)
	}
	if first.Builder == second.Builder || first.Tools == second.Tools || first.Operations == second.Operations {
		t.Fatal("shared mutable session components")
	}
	if _, ok := first.Tools.Resolve(tool.BashName); ok {
		t.Fatal("mutated caller tool list affected runtime")
	}
	if _, ok := first.Tools.Resolve(tool.ReadName); !ok {
		t.Fatal("default native tools absent")
	}
	for _, name := range []string{"ast_grep", "ast_edit", "LSP", "DAP"} {
		if _, ok := first.Tools.Resolve(name); !ok {
			t.Fatalf("missing integrated tool %s", name)
		}
	}
	cancel()
	first.Close()
	second.Close()
	owner, err := host.New(t.Context(), host.Config{Directory: c.SessionDirectory, Build: factory})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	// Configuration mismatch is checked before building a model or acquiring credentials.
	s, err := owner.Create(t.Context(), host.Options{ID: "identity", Configuration: raw})
	if err != nil {
		t.Fatal(err)
	}
	owner.Close()
	<-s.Done()
	saved.SystemPrompt = "changed"
	changed, _ := json.Marshal(saved)
	replacement, err := host.New(t.Context(), host.Config{Directory: c.SessionDirectory, Build: factory})
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	if _, err = replacement.Resume(t.Context(), host.Options{ID: "identity", Configuration: changed}); err == nil {
		t.Fatal("prompt drift allowed on resume")
	}
}

func TestInitialRuntimeUsesKnownCatalogWindow(t *testing.T) {
	registry, err := provider.New(provider.Defaults(map[string][]provider.Model{"ollama": {{ID: "synthetic-model", Family: "custom"}}})...)
	if err != nil {
		t.Fatal(err)
	}
	for _, window := range []int64{0, 32768, 272000} {
		t.Run(fmt.Sprint(window), func(t *testing.T) {
			identity := RuntimeIdentity{Version: 1, Workspace: t.TempDir(), Provider: provider.Selection{Version: 1, Provider: "ollama", Model: provider.Model{ID: "synthetic-model", Family: "custom"}, Auth: credential.Reference{Method: credential.None}, Source: "test", MaxAttempts: 1}, Profile: profile.Default(), ReasoningEffort: llm.ReasoningEffortLow}
			factory, _, err := NewRuntimeFactory(RuntimeConfig{Identity: identity, SessionDirectory: t.TempDir(), Providers: registry, ModelCatalog: func(context.Context) (modelcatalog.Catalog, error) {
				return modelcatalog.Catalog{Available: true, Models: []modelcatalog.Model{{ID: "synthetic-model", ContextWindow: window}}}, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(permission.WithPolicy(t.Context(), permission.Unrestricted()))
			defer cancel()
			r, err := factory(ctx, "catalog-fixture")
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			if r.Selection.ContextWindow != window {
				t.Fatal("initial runtime ignored trusted catalog window")
			}
			built, err := r.Builder.Build()
			if err != nil {
				t.Fatal(err)
			}
			if window > 0 && built.Report.Context.Budget.Window != window {
				t.Fatal("initial context budget ignored catalog window")
			}
		})
	}
}

func TestInitialCatalogWindowIsCanonicalThroughHostResume(t *testing.T) {
	registry, err := provider.New(provider.Defaults(map[string][]provider.Model{"ollama": {{ID: "synthetic-model"}}})...)
	if err != nil {
		t.Fatal(err)
	}
	identity := RuntimeIdentity{Version: 1, Workspace: t.TempDir(), Provider: provider.Selection{Version: 1, Provider: "ollama", Model: provider.Model{ID: "synthetic-model"}, Auth: credential.Reference{Method: credential.None}, Source: "test", MaxAttempts: 1}, Profile: profile.Default(), ReasoningEffort: llm.ReasoningEffortLow}
	window := int64(272000)
	dir := t.TempDir()
	factory, raw, err := NewRuntimeFactory(RuntimeConfig{Identity: identity, SessionDirectory: dir, Providers: registry, ModelCatalog: func(context.Context) (modelcatalog.Catalog, error) {
		return modelcatalog.Catalog{Available: true, Models: []modelcatalog.Model{{ID: "synthetic-model", ContextWindow: window}}}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	h, err := host.New(t.Context(), host.Config{Directory: dir, Build: factory})
	if err != nil {
		t.Fatal(err)
	}
	s, err := h.Create(t.Context(), host.Options{ID: "initial-window", Configuration: raw})
	if err != nil {
		t.Fatal(err)
	}
	a, pending := s.RuntimeSelection()
	if a == nil || a.ContextWindow != window || pending != nil {
		t.Fatal("Host configuration projection discarded initial catalog metadata")
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	<-s.Done()
	window = 65536 // external metadata changes must not rewrite old identity
	h, err = host.New(t.Context(), host.Config{Directory: dir, Build: factory})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	resumed, err := h.Resume(t.Context(), host.Options{ID: s.ID, Configuration: raw})
	if err != nil {
		t.Fatal(err)
	}
	after, pending := resumed.RuntimeSelection()
	if after == nil || *after != *a || pending != nil {
		t.Fatal("resume changed canonical window or runtime identity")
	}
}

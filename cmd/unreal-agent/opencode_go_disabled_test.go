//go:build disable_opencode_go && (linux || darwin)

package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/cmd/internal/authcli"
	"github.com/unreallabsai/unreal-agent/harness/authflow"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/opencodego"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/profile"
	"github.com/unreallabsai/unreal-agent/harness/provider"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	"github.com/unreallabsai/unreal-agent/harness/subagent"
	"github.com/unreallabsai/unreal-agent/internal/buildfeatures"
)

func requireGoDisabled(t *testing.T, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), buildfeatures.OpenCodeGoDisabledCode) || !strings.Contains(err.Error(), "この配布物では") {
		t.Fatalf("expected distributor refusal, got %v", err)
	}
}

type noGoTransport struct{ t *testing.T }

func (n noGoTransport) RoundTrip(*http.Request) (*http.Response, error) {
	n.t.Fatal("disabled OpenCode Go attempted HTTP")
	return nil, errors.New("unexpected HTTP")
}

func TestDisabledOpenCodeGoTransportCatalogRegistryAndAuth(t *testing.T) {
	t.Setenv("OPENCODE_GO_ENABLED", "true")
	t.Setenv("OPENCODE_API_KEY", "synthetic-secret-never-print")
	hc := &http.Client{Transport: noGoTransport{t}}
	_, err := opencodego.NewClient(opencodego.Config{APIKey: "synthetic-secret-never-print", HTTPClient: hc})
	requireGoDisabled(t, err)
	if len(opencodego.OfferedModels()) != 0 {
		t.Fatal("disabled offered models remain")
	}
	for _, catalog := range []modelcatalog.Catalog{
		modelcatalog.ReadOpenCodeGo(t.Context(), hc, opencodego.ModelsURL, []string{"m"}),
		modelcatalog.ParseOpenCodeGo([]byte(`{"data":[{"id":"m"}]}`), []string{"m"}),
	} {
		if catalog.Available || len(catalog.Models) != 0 || catalog.ProblemCode != buildfeatures.OpenCodeGoDisabledCode {
			t.Fatal("disabled catalog", catalog)
		}
	}
	descriptors := provider.Defaults(nil)
	for _, id := range []string{"openai", "openai-codex", "claude", "claude-api", "ollama", "openrouter", "fireworks"} {
		if !slices.ContainsFunc(descriptors, func(d provider.Descriptor) bool { return d.ID == id }) {
			t.Fatal("unrelated provider removed", id)
		}
	}
	if slices.ContainsFunc(descriptors, func(d provider.Descriptor) bool { return d.ID == opencodego.ProviderID }) {
		t.Fatal("disabled descriptor remains")
	}
	r, err := provider.New(append(descriptors, provider.Descriptor{ID: opencodego.ProviderID, New: func(provider.BuildConfig, credential.Material) (provider.Client, error) {
		t.Fatal("disabled factory called")
		return nil, nil
	}})...)
	if err != nil {
		t.Fatal(err)
	}
	selection := openCodeGoRoute("saved-model")
	_, err = r.Resolve(selection)
	requireGoDisabled(t, err)
	_, _, err = r.Build(provider.BuildConfig{Selection: selection, Resolver: credential.ResolverFunc(func(context.Context, credential.Reference) (credential.Material, error) {
		t.Fatal("disabled credential read")
		return credential.Material{}, nil
	})})
	requireGoDisabled(t, err)
	for _, support := range authflow.Matrix() {
		if support.Provider == opencodego.ProviderID {
			t.Fatal("disabled auth method remains")
		}
	}
	store := filepath.Join(t.TempDir(), "uncreated")
	var out bytes.Buffer
	err = authcli.Run(t.Context(), []string{"login", "--store", store, "--provider", opencodego.ProviderID, "--id", "primary", "--key-stdin"}, strings.NewReader("synthetic-secret-never-print"), &out)
	requireGoDisabled(t, err)
	if _, err := os.Stat(store); !errors.Is(err, os.ErrNotExist) || out.Len() != 0 {
		t.Fatal("refusal created a store or printed output")
	}
}

func TestDisabledOpenCodeGoDiscoveryEnvironmentAndRefresh(t *testing.T) {
	original := readOpenCodeGoModels
	readOpenCodeGoModels = func(context.Context) modelcatalog.Catalog {
		t.Fatal("disabled discovery read models")
		return modelcatalog.Catalog{}
	}
	t.Cleanup(func() { readOpenCodeGoModels = original })
	// A legacy key is deliberately written below authflow, as it would have
	// been by an earlier enabled installation. All paths are synthetic.
	directory := filepath.Join(t.TempDir(), "credentials")
	store, err := credential.OpenLocal(directory)
	if err != nil {
		t.Fatal(err)
	}
	if err = credential.NewManager(store).Login(t.Context(), openCodeGoKey, credential.Material{Token: credential.NewSecret("synthetic-old-key"), Owner: credential.Managed}); err != nil {
		t.Fatal(err)
	}
	config := serveConfiguration{Launcher: &normalLauncherConfig{Version: 1, CodexDisabled: true, ClaudeDisabled: true}, Providers: map[string]agentrunner.ProviderRuntime{opencodego.ProviderID: {Provider: openCodeGoRoute("saved-model")}}}
	config, err = registerNormalProviders(t.Context(), config, "", directory, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(config.providerStatuses, func(p modelcatalog.Provider) bool { return p.ID == opencodego.ProviderID }) {
		t.Fatal("disabled provider discovered")
	}
	live := &agentrunner.DiscoveredBackends{}
	if err := normalRediscovery(config, "", func() string { return directory }, live, func(string) string { return "" }, nil)(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, ok := live.Lookup(opencodego.ProviderID); ok {
		t.Fatal("refresh registered disabled provider")
	}
	_, err = discoverOpenCodeGo(t.Context())
	requireGoDisabled(t, err)
	_, err = bootstrapRuntime(t.Context(), launchOptions{bootstrapCodexDisabled: true, bootstrapClaudeDisabled: true}, nil, func(key string) string {
		if key == "UNREAL_HARNESS_LLM_PROVIDER" {
			return opencodego.ProviderID
		}
		return ""
	})
	requireGoDisabled(t, err)
}

func TestDisabledOpenCodeGoSavedSessionUIAndChildren(t *testing.T) {
	for _, creationGo := range []bool{true, false} {
		t.Run(map[bool]string{true: "creation-config", false: "applied-selection"}[creationGo], func(t *testing.T) {
			directory := t.TempDir()
			initial := loginRuntime().Provider
			if creationGo {
				initial = openCodeGoRoute("saved-model")
			}
			identity := agentrunner.RuntimeIdentity{Version: 1, Provider: initial, Workspace: directory, Profile: profile.Default(), ReasoningEffort: "high"}
			registry, err := provider.New(loginDescriptor())
			if err != nil {
				t.Fatal(err)
			}
			runtime := agentrunner.RuntimeConfig{Identity: identity, SessionDirectory: directory, Providers: registry,
				Backends: map[string]agentrunner.ProviderRuntime{loginProvider: loginRuntime()}}
			if !creationGo {
				delete(runtime.Backends, loginProvider)
			}
			factory, raw, err := agentrunner.NewRuntimeFactory(runtime)
			if err != nil {
				t.Fatal(err)
			}
			owner, err := host.New(t.Context(), host.Config{Directory: directory, Build: factory})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := owner.Create(t.Context(), host.Options{ID: "saved", Configuration: raw}); err != nil {
				owner.Close()
				t.Fatal(err)
			}
			if err := owner.Close(); err != nil {
				t.Fatal(err)
			}
			if !creationGo {
				store, err := localfile.New(directory)
				if err != nil {
					t.Fatal(err)
				}
				choice := sessionstore.RuntimeSelection{Version: 1, Provider: opencodego.ProviderID, Model: "saved-model", Name: "Saved Go", Effort: "high", RequestID: "legacy", Revision: 1, Binding: agentrunner.ProviderRuntime{Provider: openCodeGoRoute("saved-model")}.Binding()}
				for _, kind := range []string{sessionstore.HostRuntimeSelection, sessionstore.HostRuntimeApplied} {
					if err := store.AppendHostRecord(t.Context(), "saved", sessionstore.HostRecord{Version: 1, Kind: kind, Selection: &choice}); err != nil {
						t.Fatal(err)
					}
				}
			}
			build := factory
			var resumedRuntime host.Runtime
			factory = func(ctx context.Context, id session.ID) (host.Runtime, error) {
				r, err := build(ctx, id)
				resumedRuntime = r
				return r, err
			}
			owner, err = host.New(t.Context(), host.Config{Directory: directory, Build: factory})
			if err != nil {
				t.Fatal(err)
			}
			defer owner.Close()
			s, err := owner.Resume(t.Context(), host.Options{ID: "saved", Configuration: raw})
			if err != nil {
				t.Fatal("saved Session could not open", err)
			}
			active, _ := s.RuntimeSelection()
			if active == nil || active.Provider != opencodego.ProviderID {
				t.Fatal("saved selection silently changed", active)
			}
			modelFile := filepath.Join(directory, "subagent-model.json")
			if err := writeSubagentModel(modelFile, subagentModel{Version: 1, Provider: opencodego.ProviderID, Model: "saved-model"}); err != nil {
				t.Fatal(err)
			}
			ui := modelUI{owner: owner, runtime: runtime, subagentModel: subagentModelScope{defaultFile: modelFile, sessionDirectory: directory}}
			for _, providers := range [][]modelcatalog.Provider{ui.providers(t.Context()), ui.subagentProviders(t.Context())} {
				if slices.ContainsFunc(providers, func(p modelcatalog.Provider) bool { return p.ID == opencodego.ProviderID }) {
					t.Fatal("disabled provider offered", providers)
				}
			}
			for _, action := range []string{"model.catalog", "model.select", "subagent.select"} {
				request, _ := json.Marshal(modelRequest{Action: action, ID: "saved", Provider: opencodego.ProviderID, Selection: *active})
				out := httptest.NewRecorder()
				ui.ServeHTTP(out, httptest.NewRequest("POST", "/", bytes.NewReader(request)))
				var reply modelReply
				if err := json.Unmarshal(out.Body.Bytes(), &reply); err != nil || reply.Error != buildfeatures.OpenCodeGoDisabledCode || !strings.Contains(reply.Hint, "この配布物では") {
					t.Fatal(action, reply, err)
				}
			}
			resolver := childRuntimeResolver(runtime, modelFile)
			for _, requested := range []*subagent.RuntimeRequest{nil, {Provider: opencodego.ProviderID, Model: "saved-model", Effort: "high"}} {
				_, err := resolver(t.Context(), s, "worker-high", subagent.Template{Default: &subagent.RuntimeRequest{Provider: "openai-codex", Model: "m", Effort: "high"}}, requested)
				requireGoDisabled(t, err)
			}
			childRaw, _ := json.Marshal(agentrunner.RuntimeIdentity{Version: 1, Provider: openCodeGoRoute("saved-model"), Workspace: directory})
			_, _, err = agentrunner.NewChildFactory(runtime, nil)(t.Context(), subagent.ChildConfig{Runtime: childRaw, Workspace: directory}, subagent.ChildLink{})
			requireGoDisabled(t, err)
			_, err = resumedRuntime.LLM.Respond(t.Context(), llm.Request{}, llm.RequestOptions{})
			requireGoDisabled(t, err)
			// A saved disabled route can be inspected and explicitly replaced.
			choice, err := runtime.BindSelection(sessionstore.RuntimeSelection{Version: 1, Provider: loginProvider, Model: loginModel, Effort: "medium", RequestID: "replace"})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.SelectRuntime(t.Context(), s.Generation, active.Revision, choice); err != nil {
				t.Fatal("explicit replacement failed", err)
			}
			still, pending := s.RuntimeSelection()
			if still.Provider != opencodego.ProviderID || pending == nil || pending.Provider != loginProvider {
				t.Fatal("replacement/fallback semantics", still, pending)
			}

		})
	}
}

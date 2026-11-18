package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"path/filepath"
	"testing"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/opencodego"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/subagent"
)

func TestChildRoleSelectionFollowsResolvedLayer(t *testing.T) {
	cfg := codexServeConfiguration(t.TempDir(), "https://codex.invalid")
	cfg.Providers = map[string]agentrunner.ProviderRuntime{opencodego.ProviderID: {Provider: openCodeGoRoute("deepseek-v4.1-flash")}}
	registry, err := registeredProviders(cfg.Runtime, cfg.Providers)
	if err != nil {
		t.Fatal(err)
	}
	c := agentrunner.RuntimeConfig{
		Identity: cfg.Runtime, Providers: registry, Backends: cfg.Providers, SessionDirectory: t.TempDir(), BuiltinSubagents: true,
		Credentials: credential.ResolverFunc(func(context.Context, credential.Reference) (credential.Material, error) {
			return credential.Material{}, nil
		}),
		Catalogs: map[string]func(context.Context) (modelcatalog.Catalog, error){},
	}
	for id, model := range map[string]string{"openai-codex": leadModel, opencodego.ProviderID: "deepseek-v4.1-flash"} {
		c.Catalogs[id] = func(context.Context) (modelcatalog.Catalog, error) {
			return modelcatalog.Catalog{Available: true, Authoritative: true, Models: []modelcatalog.Model{{ID: model, Efforts: []llm.ReasoningEffort{"low", "high", "max"}, DefaultEffort: "high"}}}, nil
		}
	}
	build, raw, err := agentrunner.NewRuntimeFactory(c)
	if err != nil {
		t.Fatal(err)
	}
	h, err := host.New(t.Context(), host.Config{Directory: c.SessionDirectory, Build: build})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	parent, err := h.Create(t.Context(), host.Options{ID: "parent", Configuration: raw, Policy: permission.Unrestricted()})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "subagent-model.json")
	saved := subagentModel{Version: 1, Provider: "openai-codex"}
	saved.setRole("lead", subagentModelChoice{Provider: "openai-codex", Model: leadModel, Effort: "max"})
	for _, role := range []string{"worker", "collector"} {
		saved.setRole(role, subagentModelChoice{Provider: opencodego.ProviderID, Model: "deepseek-v4.1-flash", Effort: "max"})
	}
	if err := writeSubagentModel(path, saved); err != nil {
		t.Fatal(err)
	}
	for _, parentLayer := range []string{"main", "lead", "worker", "collector"} {
		for _, name := range []string{"worker-low", "worker-max", "collector"} {
			t.Run(parentLayer+"/"+name, func(t *testing.T) {
				identity := cfg.Runtime
				identity.SystemPrompt = workerPrompt
				if name == "collector" {
					identity.SystemPrompt = collectorPrompt
				}
				_, runtime, err := agentrunner.NewRuntimeFactory(agentrunner.RuntimeConfig{Identity: identity, Providers: registry, SessionDirectory: c.SessionDirectory, Credentials: c.Credentials})
				if err != nil {
					t.Fatal(err)
				}
				base := subagent.Template{Workspace: identity.Workspace, Runtime: runtime, Policy: permission.Config{Tools: []string{"Finish"}}, Default: &subagent.RuntimeRequest{Provider: "openai-codex", Model: collectorModel, Effort: "low"}}
				resolve := childRuntimeResolverForLayer(c, path, parentLayer, nil)
				chosen, err := resolve(t.Context(), parent, name, base, nil)
				if err != nil {
					t.Fatal(err)
				}
				wantProvider, wantModel, wantLayer := opencodego.ProviderID, "deepseek-v4.1-flash", subagent.RoleGroup(name)
				if parentLayer == "main" {
					wantProvider, wantModel, wantLayer = "openai-codex", leadModel, "lead"
				}
				selection := sessionstore.SelectionFromConfiguration(chosen.Runtime)
				if chosen.Layer != wantLayer || selection == nil || selection.Provider != wantProvider || selection.Model != wantModel || selection.Effort != "max" {
					t.Fatalf("layer=%q selection=%+v; want %s / %s / %s max", chosen.Layer, selection, wantLayer, wantProvider, wantModel)
				}
				if err := childTemplateValidator(c)(base, chosen); err != nil {
					t.Fatal("recorded layer/runtime rejected during replay", err)
				}
				// Full per-start runtime requests keep their existing selection rules.
				explicit := &subagent.RuntimeRequest{Provider: opencodego.ProviderID, Model: "deepseek-v4.1-flash", Effort: "low"}
				chosen, err = resolve(t.Context(), parent, name, base, explicit)
				selection = sessionstore.SelectionFromConfiguration(chosen.Runtime)
				if err != nil || chosen.Layer != wantLayer || selection == nil || selection.Provider != explicit.Provider || selection.Model != explicit.Model || selection.Effort != explicit.Effort {
					t.Fatalf("explicit request changed: %+v layer=%q err=%v", selection, chosen.Layer, err)
				}
				// The optional layer does not invalidate old recorded configurations.
				legacy := chosen
				legacy.Layer = ""
				if err := childTemplateValidator(c)(base, legacy); err != nil {
					t.Fatal("legacy start rejected", err)
				}
				// An unavailable worker route must neither block a direct lead
				// selection nor fall back to Codex below the lead.
				unavailable := c
				unavailable.Health = map[string]func(context.Context) error{opencodego.ProviderID: func(context.Context) error { return errors.New("worker disconnected") }}
				resolved, err := childRuntimeResolverForLayer(unavailable, path, parentLayer, nil)(t.Context(), parent, name, base, nil)
				if parentLayer == "main" {
					selection := sessionstore.SelectionFromConfiguration(resolved.Runtime)
					if err != nil || selection == nil || selection.Provider != "openai-codex" || selection.Model != leadModel || selection.Effort != "max" {
						t.Fatal("unavailable worker blocked a direct lead selection", selection, err)
					}
				} else {
					var refusal *subagent.RuntimeError
					if !errors.As(err, &refusal) || refusal.Code != "provider_unavailable" {
						t.Fatal("unavailable child role fell back", err)
					}
				}
				encoded, err := json.Marshal(chosen)
				if err != nil {
					t.Fatal(err)
				}
				var decoded subagent.Template
				if err := json.Unmarshal(encoded, &decoded); err != nil || decoded.Layer != wantLayer {
					t.Fatal("layer lost on persistence", err)
				}
			})
		}
	}
}

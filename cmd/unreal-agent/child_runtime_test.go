package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudecode"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/subagent"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
	"path/filepath"
	"testing"
)

func TestChildRuntimeAuthorizationUsesOwnerPolicyAndPreservesTemplate(t *testing.T) {
	f := testclaude.New(t)
	f.Set(t, testclaude.Config{Subscription: "team"})
	cfg := claudeServeConfiguration(t, f)
	cfg.Runtime.ClaudeCode.ManagedPolicyMode = claudecode.ManagedPolicyTrust
	// A parent's explicitly pinned model may be catalogued under an alias.
	// Inheritance keeps the pinned selection instead of resolving it backwards.
	cfg.Runtime.Provider.Model.ID = "configured-resolved-a"
	cfg.Runtime.ClaudeCode.Models[0].ResolvedModel = cfg.Runtime.Provider.Model.ID
	registry, e := runtimeProviders(cfg.Runtime)
	if e != nil {
		t.Fatal(e)
	}
	c := agentrunner.RuntimeConfig{Identity: cfg.Runtime, Providers: registry, SessionDirectory: filepath.Join(t.TempDir(), "sessions"), Catalogs: map[string]func(context.Context) (modelcatalog.Catalog, error){"claude-code": func(context.Context) (modelcatalog.Catalog, error) {
		return modelcatalog.Configured(cfg.Runtime.ClaudeCode.Models)
	}}}
	factory, identity, e := agentrunner.NewRuntimeFactory(c)
	if e != nil {
		t.Fatal(e)
	}
	if e = json.Unmarshal(identity, &c.Identity); e != nil {
		t.Fatal(e)
	}
	owner, e := host.New(t.Context(), host.Config{Directory: c.SessionDirectory, Build: factory})
	if e != nil {
		t.Fatal(e)
	}
	defer owner.Close()
	denied, e := owner.Create(t.Context(), host.Options{ID: "denied", Policy: permission.DenyAll(), Configuration: identity})
	if e != nil {
		t.Fatal(e)
	}
	template := subagent.Template{Workspace: c.Identity.Workspace, Runtime: identity, Policy: permission.Config{Tools: []string{"Finish"}}}
	resolve := childRuntimeResolver(c)
	_, e = resolve(permission.WithPolicy(t.Context(), permission.Unrestricted()), denied, template, nil)
	var re *subagent.RuntimeError
	if !errors.As(e, &re) || re.Code != "permission_denied" {
		t.Fatal("caller widened owner policy", e)
	}
	parentPolicy, e := permission.New(permission.Config{Tools: []string{"SubagentStart"}, ProcessMode: permission.ProcessUnrestricted, FilesystemUnrestricted: true, NetworkUnrestricted: true})
	if e != nil {
		t.Fatal(e)
	}
	defer parentPolicy.Close()
	limited, e := owner.Create(t.Context(), host.Options{ID: "limited", Policy: parentPolicy, Configuration: identity})
	if e != nil {
		t.Fatal(e)
	}
	_, e = resolve(t.Context(), limited, template, nil)
	if !errors.As(e, &re) || re.Code != "permission_denied" {
		t.Fatal("child expanded tool allowlist", e)
	}
	template.Policy.Tools = nil
	chosen, e := resolve(t.Context(), limited, template, nil)
	if e != nil {
		t.Fatal(e)
	}
	a := sessionstore.SelectionFromConfiguration(chosen.Runtime)
	if a.Provider != "claude-code" || a.Model != c.Identity.Provider.Model.ID {
		t.Fatal("initial inheritance")
	}
	verify := childTemplateValidator(c)
	if e = verify(template, chosen); e != nil {
		t.Fatal(e)
	}
	// Template validation sees the operator's original catalog too. Omitted
	// display names are normalized by the same factory, not a resume mismatch.
	unnamed := c
	copyConfig := *c.Identity.ClaudeCode
	copyConfig.Models = append([]modelcatalog.Model(nil), copyConfig.Models...)
	for i := range copyConfig.Models {
		copyConfig.Models[i].Name = ""
	}
	unnamed.Identity.ClaudeCode = &copyConfig
	normalizedChild, e := childRuntimeResolver(unnamed)(t.Context(), limited, template, nil)
	if e != nil {
		t.Fatal(e)
	}
	if e = childTemplateValidator(unnamed)(template, normalizedChild); e != nil {
		t.Fatal("normalized catalog names broke child recovery", e)
	}
	if e = normalizedChild.Runtime.Canonicalize(); e != nil {
		t.Fatal(e)
	}
	if e = childTemplateValidator(unnamed)(template, normalizedChild); e != nil {
		t.Fatal("protocol JSON key order broke child recovery", e)
	}
	corrupt := chosen
	corrupt.Policy.Tools = []string{"Bash"}
	if e = verify(template, corrupt); e == nil {
		t.Fatal("recovery authorized widened permissions")
	}
	corrupt = chosen
	var runtime agentrunner.RuntimeIdentity
	_ = json.Unmarshal(corrupt.Runtime, &runtime)
	runtime.SystemPrompt = "unapproved prompt"
	corrupt.Runtime, _ = json.Marshal(runtime)
	if e = verify(template, corrupt); e == nil {
		t.Fatal("runtime selector changed template instructions")
	}
	// A configured catalog is not evidence that the CLI can execute. The
	// provider's independent non-inference health check remains authoritative.
	c.Health = map[string]func(context.Context) error{"claude-code": func(context.Context) error {
		return &claudecode.Error{Code: "unsupported_version"}
	}}
	_, e = childRuntimeResolver(c)(t.Context(), limited, template, nil)
	if !errors.As(e, &re) || re.Code != "provider_unavailable" {
		t.Fatal("catalog fallback hid unavailable child transport", e)
	}
	deniedProcessPolicy, e := permission.New(permission.Config{Tools: []string{"SubagentStart"}})
	if e != nil {
		t.Fatal(e)
	}
	defer deniedProcessPolicy.Close()
	noProcess, e := owner.Create(t.Context(), host.Options{ID: "no-process", Policy: deniedProcessPolicy, Configuration: identity})
	if e != nil {
		t.Fatal(e)
	}
	c.Health["claude-code"] = func(context.Context) error {
		t.Error("CLI availability probe ran without owner process permission")
		return nil
	}
	_, e = childRuntimeResolver(c)(t.Context(), noProcess, template, nil)
	if !errors.As(e, &re) || re.Code != "permission_denied" {
		t.Fatal("child expanded parent process permission", e)
	}
	c.Health = nil
	c.Catalogs["claude-code"] = func(context.Context) (modelcatalog.Catalog, error) {
		return modelcatalog.Catalog{}, &claudecode.Error{Code: "external_reauth_required"}
	}
	_, e = childRuntimeResolver(c)(t.Context(), limited, template, &subagent.RuntimeRequest{Provider: "claude-code", Model: a.Model, Effort: llm.ReasoningEffortMedium})
	if !errors.As(e, &re) || re.Code != "provider_unavailable" {
		t.Fatal("unavailable child provider allowed", e)
	}
	if len(f.Calls(t)) != 0 {
		t.Fatal("runtime authorization sent inference")
	}
}

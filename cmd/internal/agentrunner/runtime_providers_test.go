package agentrunner

import (
	"context"
	"encoding/json/v2"
	"errors"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudecode"
	"github.com/unreallabsai/unreal-agent/harness/profile"
	"github.com/unreallabsai/unreal-agent/harness/provider"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"path/filepath"
	"testing"
)

type unavailableRuntimeClient struct{}

func (unavailableRuntimeClient) Close() error { return nil }
func (unavailableRuntimeClient) Respond(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
	return llm.Response{}, &claudecode.Error{Code: "external_reauth_required"}
}
func TestMultiProviderBindingCapabilitiesAndTypedFailure(t *testing.T) {
	a := provider.Selection{Version: 1, Provider: "openai-codex", Model: provider.Model{ID: "shared"}, Auth: credential.Reference{Provider: "openai-codex", Method: credential.OAuth, ID: "external-codex"}, MaxAttempts: 1, Source: "test"}
	b := provider.Selection{Version: 1, Provider: "claude-code", Model: provider.Model{ID: "shared"}, Auth: credential.Reference{Provider: "claude-code", Method: credential.OAuth, ID: "external-claude-code"}, MaxAttempts: 1, Source: "test"}
	descriptors := provider.Defaults(map[string][]provider.Model{"openai-codex": {a.Model}, "claude-code": {b.Model}})
	for i := range descriptors {
		if descriptors[i].ID == "claude-code" {
			descriptors[i].New = func(provider.BuildConfig, credential.Material) (provider.Client, error) {
				return unavailableRuntimeClient{}, nil
			}
		}
	}
	registry, e := provider.New(descriptors...)
	if e != nil {
		t.Fatal(e)
	}
	cfg := RuntimeConfig{Identity: RuntimeIdentity{Version: 1, Provider: a, Profile: profile.Default(), ReasoningEffort: "medium", Workspace: t.TempDir()}, Providers: registry, SessionDirectory: filepath.Join(t.TempDir(), "sessions"), Credentials: credential.ResolverFunc(func(context.Context, credential.Reference) (credential.Material, error) {
		return credential.Material{Token: credential.NewSecret("secret-not-binding")}, nil
	}), Backends: map[string]ProviderRuntime{"claude-code": {Provider: b, ReasoningEffort: "medium", ClaudeCode: &claudecode.Config{ManagedPolicyMode: claudecode.ManagedPolicyTrust}}}}
	factory, identity, e := NewRuntimeFactory(cfg)
	if e != nil {
		t.Fatal(e)
	}
	_ = json.Unmarshal(identity, &cfg.Identity)
	runtime, e := factory(t.Context(), "s")
	if e != nil {
		t.Fatal(e)
	}
	defer runtime.Close()
	before, _ := runtime.Builder.Build()
	if len(before.Request.Tools) == 0 {
		t.Fatal("Codex registry lost")
	}
	choice := sessionstore.RuntimeSelection{Version: 1, Provider: "claude-code", Model: "shared", Effort: "medium"}
	if runtime.ValidateSelection(choice) == nil {
		t.Fatal("unbound provider change accepted")
	}
	choice, e = cfg.BindSelection(choice)
	if e != nil {
		t.Fatal(e)
	}
	if e = runtime.ApplySelection(choice); e != nil {
		t.Fatal(e)
	}
	text, _ := runtime.Builder.Build()
	if len(text.Request.Tools) != 0 || len(runtime.Tools.StaticDefinitions()) != 0 {
		t.Fatal("Claude capability did not switch")
	}
	_, e = runtime.LLM.Respond(t.Context(), text.Request, llm.RequestOptions{})
	var failure *claudecode.Error
	if !errors.As(e, &failure) || failure.Code != "external_reauth_required" {
		t.Fatal("typed provider failure lost", e)
	}
	// A registered adapter factory must survive discovery/model changes. There
	// is no default factory fallback that could send an unavailable request elsewhere.
	choice.Model = "discovered"
	choice, e = cfg.BindSelection(choice)
	if e != nil {
		t.Fatal(e)
	}
	if e = runtime.ApplySelection(choice); e != nil {
		t.Fatal(e)
	}
	text, _ = runtime.Builder.Build()
	_, e = runtime.LLM.Respond(t.Context(), text.Request, llm.RequestOptions{})
	if !errors.As(e, &failure) {
		t.Fatal("model extension replaced adapter factory")
	}
	drift := choice
	drift.Binding = `{"different":true}`
	if runtime.ValidateSelection(drift) == nil {
		t.Fatal("provider binding drift allowed")
	}
	if e = runtime.ApplySelection(*runtime.Selection); e != nil {
		t.Fatal(e)
	}
	after, _ := runtime.Builder.Build()
	if len(after.Request.Tools) != len(before.Request.Tools) {
		t.Fatal("Codex capability did not recover")
	}
}

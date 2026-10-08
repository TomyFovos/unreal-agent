package provider

import (
	"context"
	"errors"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudecode"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"testing"
)

func TestClaudeProviderNeverObtainsCredentialMaterial(t *testing.T) {
	m := Model{ID: "configured-model", Capabilities: []string{"reasoning"}}
	r, e := New(Defaults(map[string][]Model{"claude-code": {m}})...)
	if e != nil {
		t.Fatal(e)
	}
	s := Selection{Version: 1, Provider: "claude-code", Model: m, Auth: credential.Reference{Provider: "claude-code", Method: credential.OAuth, ID: "external-claude-code"}, MaxAttempts: 1, Source: "test"}
	resolver := credential.ResolverFunc(func(context.Context, credential.Reference) (credential.Material, error) {
		t.Error("Claude credential material requested")
		return credential.Material{}, errors.New("secret")
	})
	client, selected, e := r.Build(BuildConfig{Selection: s, Resolver: resolver, ClaudeCode: claudecode.Config{Binary: "/missing/claude", Models: []modelcatalog.Model{{ID: m.ID, Efforts: []llm.ReasoningEffort{"medium"}}}}})
	if e != nil || selected.Endpoint != "" || r.RequiresResolver(selected) {
		t.Fatal("CLI treated as HTTP/managed auth", e)
	}
	// Tool rejection happens before any subprocess or external credential read.
	_, e = client.Respond(t.Context(), llm.Request{Model: llm.Model{ID: m.ID, ReasoningEffort: "medium"}, Tools: []llm.Tool{{Name: "Bash"}}}, llm.RequestOptions{})
	var typed *claudecode.Error
	if !errors.As(e, &typed) || typed.Code != "tools_unsupported" {
		t.Fatal("lost declared text-only capability", e)
	}
	for _, change := range []func(*Selection){
		func(s *Selection) { s.Endpoint = "https://api.anthropic.com" },
		func(s *Selection) { s.Auth.Method = credential.APIKey },
		func(s *Selection) { s.Auth.ID = "managed" },
		func(s *Selection) { s.Model.Capabilities = []string{"tools"} },
		func(s *Selection) { s.Model.Capabilities = []string{"images"} },
	} {
		bad := s
		change(&bad)
		if _, e = r.Resolve(bad); e == nil {
			t.Fatal("Claude subscription selection accepted API/tool fallback")
		}
	}
	missing := &claudecode.Error{Code: "external_reauth_required"}
	if got := normalize(missing); got != missing || !credential.IsCode(got, "external_reauth_required") {
		t.Fatal("typed external reauth was erased")
	}
}

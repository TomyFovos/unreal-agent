package provider

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/llm"
)

func fixtureSelection(id, endpoint string) Selection {
	method := credential.APIKey
	if id == "ollama" {
		method = credential.None
	}
	if id == "openai-codex" {
		method = credential.OAuth
	}
	auth := credential.Reference{Method: method}
	if method != credential.None {
		auth.Provider = id
		auth.ID = "test"
	}
	return Selection{Version: 1, Provider: id, Model: Model{ID: "explicit-model"}, Endpoint: endpoint, Auth: auth, MaxAttempts: 1, Source: "test catalog"}
}
func TestFiveExistingProvidersResolveAtRequestTime(t *testing.T) {
	for _, id := range []string{"openai", "openrouter", "fireworks", "ollama", "openai-codex"} {
		t.Run(id, func(t *testing.T) {
			var generation atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if id != "ollama" && r.Header.Get("Authorization") != fmt.Sprintf("Bearer dynamic-%d", generation.Load()) {
					t.Error("stale credentials")
				}
				if id == "openai-codex" && r.Header.Get("ChatGPT-Account-ID") != "account" {
					t.Error("missing account")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, `data: {"type":"response.completed","response":{"id":"r","object":"response","status":"completed","output":[{"type":"function_call","id":"f","call_id":"c","name":"read","arguments":"{}","status":"completed"}],"usage":{"input_tokens":1,"output_tokens":1}}}`+"\n\n")
			}))
			defer server.Close()
			reg, err := New(Defaults(map[string][]Model{id: {{ID: "explicit-model"}}})...)
			if err != nil {
				t.Fatal(err)
			}
			c, selection, err := reg.Build(BuildConfig{Selection: fixtureSelection(id, server.URL), Resolver: credential.ResolverFunc(func(context.Context, credential.Reference) (credential.Material, error) {
				return credential.Material{Token: credential.NewSecret(fmt.Sprintf("dynamic-%d", generation.Add(1))), AccountID: "account"}, nil
			})})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			for range 2 {
				out, err := c.Respond(t.Context(), llm.Request{Model: llm.Model{ID: selection.Model.ID}}, llm.RequestOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if len(out.Output) != 1 || out.Output[0].Type != llm.ItemToolCall {
					t.Fatalf("normalized tool call lost: %+v", out)
				}
			}
			if id != "ollama" && generation.Load() != 2 {
				t.Fatal("did not resolve each request")
			}
		})
	}
}
func TestSelectionRejectsUnknownAndResumeChanges(t *testing.T) {
	reg, _ := New(Defaults(map[string][]Model{"openai": {{ID: "m", Family: "family", Capabilities: []string{"tools"}}}})...)
	base := fixtureSelection("openai", "https://api.example")
	base.Model.ID = "m"
	selected, err := reg.Resolve(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Selection){
		func(s *Selection) { s.Provider = "other" }, func(s *Selection) { s.Model.ID = "unknown" }, func(s *Selection) { s.Auth.Method = credential.OAuth },
		func(s *Selection) { s.Model.Capabilities = []string{"web"} }, func(s *Selection) { s.Endpoint = "https://secret@api.example" }, func(s *Selection) { s.Version = 2 },
	} {
		bad := base
		change(&bad)
		if _, err := reg.Resolve(bad); err == nil {
			t.Fatalf("accepted unsupported selection %+v", bad)
		}
	}
	if ValidateResume(selected, selected) != nil {
		t.Fatal("same config incompatible")
	}
	other := selected
	other.Auth.ID = "other"
	if ValidateResume(selected, other) == nil {
		t.Fatal("credential silently switched")
	}
	selected.Model.Capabilities[0] = "changed"
	again, _ := reg.Resolve(base)
	if again.Model.Capabilities[0] != "tools" {
		t.Fatal("selection aliases catalog")
	}
}
func TestErrorsRedactedNoAuthenticationReplay(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		fmt.Fprint(w, `{"error":{"message":"echoed-sensitive-token"}}`)
	}))
	defer srv.Close()
	reg, _ := New(Defaults(map[string][]Model{"openai": {{ID: "explicit-model"}}})...)
	c, _, err := reg.Build(BuildConfig{Selection: fixtureSelection("openai", srv.URL), Resolver: credential.ResolverFunc(func(context.Context, credential.Reference) (credential.Material, error) {
		return credential.Material{Token: credential.NewSecret("ephemeral")}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Respond(t.Context(), llm.Request{Model: llm.Model{ID: "explicit-model"}}, llm.RequestOptions{})
	var p *Error
	if !errors.As(err, &p) || p.Code != "unauthenticated" || strings.Contains(err.Error(), "sensitive") || calls.Load() != 1 {
		t.Fatalf("bad normalized outcome: %v calls=%d", err, calls.Load())
	}
}
func TestInjectedTransportAndRedirectProtection(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("credential followed redirect") }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer source.Close()
	reg, _ := New(Defaults(map[string][]Model{"openai": {{ID: "explicit-model"}}})...)
	config := BuildConfig{Selection: fixtureSelection("openai", source.URL), Resolver: credential.ResolverFunc(func(context.Context, credential.Reference) (credential.Material, error) {
		return credential.Material{Token: credential.NewSecret("ephemeral")}, nil
	})}
	c, _, _ := reg.Build(config)
	_, err := c.Respond(t.Context(), llm.Request{Model: llm.Model{ID: "explicit-model"}}, llm.RequestOptions{})
	if err == nil {
		t.Fatal("redirect accepted")
	}
	var invoked atomic.Int64
	config.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { invoked.Add(1); return nil, errors.New("policy denied") })}
	c, _, _ = reg.Build(config)
	_, err = c.Respond(t.Context(), llm.Request{Model: llm.Model{ID: "explicit-model"}}, llm.RequestOptions{})
	if err == nil || invoked.Load() != 1 {
		t.Fatal("injected transport bypassed")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = c.Respond(ctx, llm.Request{}, llm.RequestOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestSecretCannotAccidentallySerialize(t *testing.T) {
	s := credential.NewSecret("generated-" + t.Name())
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, out := range []string{string(data), fmt.Sprint(s), fmt.Sprintf("%+v", s), fmt.Sprintf("%#v", s)} {
		if strings.Contains(out, s.Reveal()) {
			t.Fatal("secret leaked")
		}
	}
}

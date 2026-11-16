package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/opencodego"
)

const openCodeModel = "deepseek-v4.1-flash"

func openCodeSelection(endpoint string) Selection {
	return Selection{Version: 1, Provider: opencodego.ProviderID, Model: Model{ID: openCodeModel}, Endpoint: endpoint, Auth: credential.Reference{Provider: opencodego.ProviderID, ID: "primary", Method: credential.APIKey}, MaxAttempts: 1, Source: "test catalog"}
}

func openCodeRegistry(t *testing.T) *Registry {
	t.Helper()
	r, err := New(Defaults(map[string][]Model{opencodego.ProviderID: {{ID: openCodeModel}}})...)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func openCodeKey(key string) credential.Resolver {
	return credential.ResolverFunc(func(context.Context, credential.Reference) (credential.Material, error) {
		return credential.Material{Token: credential.NewSecret(key)}, nil
	})
}

// Unconfigured, OpenCode Go has no models, so nothing changes for the existing
// providers. It is an API key provider with tools and no catalog switching.
func TestOpenCodeGoRegistration(t *testing.T) {
	r, err := New(Defaults(nil)...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve(openCodeSelection("")); err == nil {
		t.Fatal("unconfigured OpenCode Go model accepted")
	}
	r = openCodeRegistry(t)
	s, err := r.Resolve(openCodeSelection(""))
	if err != nil || s.Endpoint != opencodego.BaseURL {
		t.Fatal("default endpoint", s.Endpoint, err)
	}
	if !r.SupportsTools(s) || !r.CatalogModels(opencodego.ProviderID) || !r.RequiresResolver(s) {
		t.Fatal("capabilities changed")
	}
	for _, method := range []credential.Method{credential.OAuth, credential.DockerSandboxOAuth, credential.None} {
		bad := openCodeSelection("")
		bad.Auth.Method = method
		if method == credential.None {
			bad.Auth = credential.Reference{Method: method}
		}
		if _, err := r.Resolve(bad); err == nil {
			t.Fatal("accepted auth method", method)
		}
	}
}

// Mirrors TestFiveExistingProvidersResolveAtRequestTime: the stored key is
// resolved for each request, never cached by the client.
func TestOpenCodeGoResolvesKeyAtRequestTime(t *testing.T) {
	var generation atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != fmt.Sprintf("Bearer dynamic-%d", generation.Load()) || r.URL.Path != "/inference/go/openai/v1/responses" {
			t.Error("stale credentials or wrong path", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"type":"response.completed","response":{"id":"r","object":"response","status":"completed","output":[{"type":"function_call","id":"f","call_id":"c","name":"read","arguments":"{}","status":"completed"}],"usage":{"input_tokens":1,"output_tokens":1}}}`+"\n\n")
	}))
	defer server.Close()
	c, selection, err := openCodeRegistry(t).Build(BuildConfig{Selection: openCodeSelection(server.URL + "/inference/go/openai/v1"), Resolver: credential.ResolverFunc(func(context.Context, credential.Reference) (credential.Material, error) {
		return credential.Material{Token: credential.NewSecret(fmt.Sprintf("dynamic-%d", generation.Add(1)))}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for range 2 {
		out, err := c.Respond(t.Context(), llm.Request{Model: llm.Model{ID: selection.Model.ID}}, llm.RequestOptions{})
		if err != nil || len(out.Output) != 1 || out.Output[0].Type != llm.ItemToolCall {
			t.Fatalf("normalized tool call lost: %+v %v", out, err)
		}
	}
	if generation.Load() != 2 {
		t.Fatal("did not resolve each request")
	}
}

// Mirrors TestErrorsRedactedNoAuthenticationReplay.
func TestOpenCodeGoErrorsRedactedNoAuthenticationReplay(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		fmt.Fprint(w, `{"error":{"message":"echoed-sensitive-token"}}`)
	}))
	defer server.Close()
	c, _, err := openCodeRegistry(t).Build(BuildConfig{Selection: openCodeSelection(server.URL + "/inference/go/openai/v1"), Resolver: openCodeKey("ephemeral")})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Respond(t.Context(), llm.Request{Model: llm.Model{ID: openCodeModel}}, llm.RequestOptions{})
	var p *Error
	if !errors.As(err, &p) || p.Code != "unauthenticated" || p.RetryableModelFailure() || strings.Contains(err.Error(), "sensitive") || strings.Contains(err.Error(), "ephemeral") || calls.Load() != 1 {
		t.Fatalf("bad normalized outcome: %v calls=%d", err, calls.Load())
	}
}

// An exhausted allowance reaches the Host as a rate limit that the child retry
// loop does not repeat; a temporary limit stays a retryable model failure.
func TestOpenCodeGoAllowanceExhaustionStopsChildRetry(t *testing.T) {
	for retryAfter, retryable := range map[string]bool{"3600": false, "1": true} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", retryAfter)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"message":"limit"}}`)
		}))
		c, _, err := openCodeRegistry(t).Build(BuildConfig{Selection: openCodeSelection(server.URL + "/inference/go/openai/v1"), Resolver: openCodeKey("key")})
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.Respond(t.Context(), llm.Request{Model: llm.Model{ID: openCodeModel}}, llm.RequestOptions{})
		server.Close()
		var p *Error
		if !errors.As(err, &p) || p.Code != "rate_limited" || p.RetryableModelFailure() != retryable {
			t.Fatal(retryAfter, err)
		}
		if reason := p.Diagnostic.Safe().Reason; !retryable && reason != "usage_limit_reached" {
			t.Fatal("reason", reason)
		}
	}
}

// Mirrors TestInjectedTransportAndRedirectProtection, and refuses Zen and any
// other destination before a request leaves the injected transport.
func TestOpenCodeGoDestinationAndRedirectProtection(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("credential followed redirect") }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer source.Close()
	r := openCodeRegistry(t)
	c, _, err := r.Build(BuildConfig{Selection: openCodeSelection(source.URL + "/inference/go/openai/v1"), Resolver: openCodeKey("ephemeral")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Respond(t.Context(), llm.Request{Model: llm.Model{ID: openCodeModel}}, llm.RequestOptions{}); err == nil {
		t.Fatal("redirect accepted")
	}
	var invoked atomic.Int64
	injected := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		invoked.Add(1)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(`data: {"type":"response.completed","response":{"id":"r","status":"completed","output":[]}}` + "\n\n"))}, nil
	})}
	for endpoint, allowed := range map[string]bool{"": true, "https://opencode.ai/zen/v1": false, "https://opencode.ai/inference/zen/openai/v1": false, "https://api.deepseek.com/v1": false} {
		before := invoked.Load()
		c, _, err := r.Build(BuildConfig{Selection: openCodeSelection(endpoint), Resolver: openCodeKey("ephemeral"), HTTPClient: injected})
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.Respond(t.Context(), llm.Request{Model: llm.Model{ID: openCodeModel}}, llm.RequestOptions{})
		if (err == nil) != allowed || invoked.Load()-before != map[bool]int64{true: 1, false: 0}[allowed] {
			t.Fatal("destination", endpoint, err, invoked.Load()-before)
		}
	}
}

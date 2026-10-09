package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/openaicodex"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/internal/testcodex"
)

func sandboxSource(t *testing.T) openaicodex.DockerSandboxSource {
	t.Helper()
	dir := t.TempDir()
	for name, content := range map[string]string{
		"auth.json": `{"OPENAI_API_KEY":"proxy-managed"}`,
		"config.toml": `forced_login_method="api"
model_provider="sandboxd"
[model_providers.sandboxd]
name="Sandbox Proxy"
base_url="https://chatgpt.com/backend-api/codex"
experimental_bearer_token="oai-oat01-proxy-managed"
requires_openai_auth=false
`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return openaicodex.DockerSandboxSource{AuthFile: filepath.Join(dir, "auth.json"), Getenv: func(string) string { return "oauth" }}
}

type proxyTestTransport func(*http.Request) (*http.Response, error)

func (f proxyTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDockerSandboxOAuthProxyAndPerRequestValidation(t *testing.T) {
	source := sandboxSource(t)
	ref := credential.Reference{Provider: "openai-codex", ID: "docker-sandbox-oauth", Method: credential.DockerSandboxOAuth}
	resolver := DockerSandboxCodex(source)
	material, err := resolver.Resolve(t.Context(), ref)
	if err != nil || material.Owner != credential.DockerSandboxProxy || material.Token.Reveal() != "" || material.AccountID != "" || material.RefreshToken.Reveal() != "" {
		t.Fatal("proxy was represented as a real token/account", err)
	}
	// Local mock of the host proxy: it sees only a protocol sentinel from Unreal.
	// There are no account credentials or real backend connections in this test.
	var requests atomic.Int32
	var reject atomic.Bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+openaicodex.SandboxAccessSentinel || r.Header.Get("ChatGPT-Account-ID") != "" || r.URL.Path != "/backend-api/codex/responses" {
			t.Error("proxy protocol mismatch")
		}
		if reject.Load() {
			w.WriteHeader(401)
			io.WriteString(w, `{"error":{"message":"private-dummy","code":"private-dummy"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"dummy\",\"status\":\"completed\",\"output\":[]}}\n\n")
	}))
	defer proxy.Close()
	transport := proxyTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != openaicodex.BaseURL+"/responses" {
			t.Fatal("original authority changed")
		}
		copy := r.Clone(r.Context())
		copy.URL.Host = strings.TrimPrefix(proxy.URL, "http://")
		copy.URL.Scheme = "http"
		copy.Host = "" // trusted fixture forwarding, never production
		return http.DefaultTransport.RoundTrip(copy)
	})
	registry, _ := New(Defaults(map[string][]Model{"openai-codex": {{ID: "gpt-test"}}})...)
	selection := Selection{Version: 1, Provider: "openai-codex", Model: Model{ID: "gpt-test"}, Auth: ref, MaxAttempts: 1, Source: "proxy fixture"}
	client, _, err := registry.Build(BuildConfig{Selection: selection, Resolver: resolver, HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	request := llm.Request{Model: llm.Model{ID: "gpt-test"}}
	policy, err := permission.New(permission.Config{NetworkOrigins: []string{"https://chatgpt.com"}})
	if err != nil {
		t.Fatal(err)
	}
	defer policy.Close()
	ctx := permission.WithPolicy(t.Context(), policy)
	if _, err = client.Respond(ctx, request, llm.RequestOptions{}); err != nil || requests.Load() != 1 {
		t.Fatal("mocked proxy exchange failed", err)
	}
	if _, err = client.Respond(permission.WithPolicy(t.Context(), permission.DenyAll()), request, llm.RequestOptions{}); permission.Failure(err) == nil || requests.Load() != 1 {
		t.Fatal("proxy bypassed Permission", err)
	}
	reject.Store(true)
	_, err = client.Respond(ctx, request, llm.RequestOptions{})
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "unauthenticated" || requests.Load() != 2 || strings.Contains(err.Error(), "private-dummy") {
		t.Fatal("401 was retried, leaked, or lost its safe type", err)
	}
	if err = os.WriteFile(source.AuthFile, []byte(`{"OPENAI_API_KEY":"sk-private-dummy"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = client.Respond(ctx, request, llm.RequestOptions{}); !credential.IsCode(err, "docker_sandbox_oauth_unavailable") || requests.Load() != 2 {
		t.Fatal("source was not reread/fail-closed", err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = client.Respond(canceled, request, llm.RequestOptions{}); !errors.Is(err, context.Canceled) || requests.Load() != 2 {
		t.Fatal("cancel did not prevent exchange")
	}
}

func TestDockerSandboxOAuthReferenceAndMaterialIsolation(t *testing.T) {
	source := sandboxSource(t)
	ref := credential.Reference{Provider: "openai-codex", ID: "docker-sandbox-oauth", Method: credential.DockerSandboxOAuth}
	for _, other := range []credential.Reference{{Provider: "openai", ID: ref.ID, Method: ref.Method}, {Provider: ref.Provider, ID: "external-codex", Method: ref.Method}, {Provider: ref.Provider, ID: ref.ID, Method: credential.OAuth}} {
		if _, err := DockerSandboxCodex(source).Resolve(t.Context(), other); !credential.IsCode(err, "unsupported_external_source") {
			t.Fatal("wrong reference routed to proxy")
		}
	}
	if _, err := ExternalCodex(openaicodex.Config{AuthFile: source.AuthFile}).Resolve(t.Context(), ref); !credential.IsCode(err, "unsupported_external_source") {
		t.Fatal("native resolver accepted proxy")
	}
	registry, _ := New(Defaults(map[string][]Model{"openai-codex": {{ID: "gpt-test"}}})...)
	for _, material := range []credential.Material{{Owner: credential.External, Token: credential.NewSecret("dummy"), AccountID: "dummy"}, {Owner: credential.DockerSandboxProxy, AccountID: "invented"}, {Owner: credential.DockerSandboxProxy, Token: credential.NewSecret(openaicodex.SandboxAccessSentinel)}} {
		resolver := credential.ResolverFunc(func(context.Context, credential.Reference) (credential.Material, error) { return material, nil })
		client, _, err := registry.Build(BuildConfig{Selection: Selection{Version: 1, Provider: "openai-codex", Model: Model{ID: "gpt-test"}, Auth: ref, MaxAttempts: 1, Source: "proxy fixture"}, Resolver: resolver})
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.Respond(t.Context(), llm.Request{Model: llm.Model{ID: "gpt-test"}}, llm.RequestOptions{})
		client.Close()
		if !credential.IsCode(err, "invalid_docker_sandbox_material") {
			t.Fatal("mixed source material accepted", err)
		}
	}
}

func TestDockerSandboxForwardProxyProviderErrorsAreSafe(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		code   string
	}{
		{"success", http.StatusOK, ""},
		{"unauthorized", http.StatusUnauthorized, "unauthenticated"},
		{"forbidden", http.StatusForbidden, "forbidden"},
		{"missing-proxy", http.StatusOK, "proxy_unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("HTTPS_PROXY", testcodex.DockerProxyURL)
			t.Setenv("https_proxy", "")
			if test.name == "missing-proxy" {
				t.Setenv("HTTPS_PROXY", "")
			}
			proxy := testcodex.NewOAuthProxy(t, openaicodex.SandboxAccessSentinel)
			proxy.Status.Store(int32(test.status))
			source := sandboxSource(t)
			ref := credential.Reference{Provider: "openai-codex", ID: "docker-sandbox-oauth", Method: credential.DockerSandboxOAuth}
			registry, err := New(Defaults(map[string][]Model{"openai-codex": {{ID: "gpt-6.1-sol"}}})...)
			if err != nil {
				t.Fatal(err)
			}
			client, _, err := registry.Build(BuildConfig{Selection: Selection{Version: 1, Provider: "openai-codex", Model: Model{ID: "gpt-6.1-sol"}, Auth: ref, MaxAttempts: 1, Source: "OAuth forward fixture"}, Resolver: DockerSandboxCodex(source), HTTPClient: &http.Client{Transport: proxy.Transport}})
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			policy, err := permission.New(permission.Config{NetworkOrigins: []string{"https://chatgpt.com"}})
			if err != nil {
				t.Fatal(err)
			}
			defer policy.Close()
			response, err := client.Respond(permission.WithPolicy(t.Context(), policy), llm.Request{Model: llm.Model{ID: "gpt-6.1-sol", ReasoningEffort: llm.ReasoningEffortMedium}}, llm.RequestOptions{})
			var failure *Error
			if test.code == "" {
				if err != nil || response.Stop != llm.StopComplete || !proxy.RequestOK.Load() {
					t.Fatal("provider forward exchange failed")
				}
			} else if !errors.As(err, &failure) || failure.Code != test.code || strings.Contains(err.Error(), "private-fixture") || strings.Contains(err.Error(), openaicodex.SandboxAccessSentinel) {
				t.Fatal("provider error lost its safe closed type")
			}
			expected := int32(1)
			if test.code == "proxy_unavailable" {
				expected = 0
			}
			if proxy.Requests.Load() != expected || proxy.DirectRequests.Load() != 0 {
				t.Fatal("provider retried or used direct egress")
			}
		})
	}
}

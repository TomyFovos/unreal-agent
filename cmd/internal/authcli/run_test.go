package authcli

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/provider"
)

func TestLoginStoreAdapterLogoutEndToEnd(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "credentials")
	secret := rand.Text()
	var output bytes.Buffer
	args := []string{"login", "--store", directory, "--provider", "openai", "--id", "work", "--method", "api_key", "--key-stdin"}
	if err := Run(t.Context(), args, strings.NewReader(secret+"\n"), &output); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), secret) {
		t.Fatal("login leaked credential")
	}
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+secret {
			t.Error("stored credential not used")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"type":"response.completed","response":{"id":"r","object":"response","status":"completed","output":[{"type":"function_call","id":"f","call_id":"c","name":"read","arguments":"{}","status":"completed"}],"usage":{"input_tokens":2,"output_tokens":1}}}`+"\n\n")
	}))
	defer server.Close()
	backend, err := credential.OpenLocal(directory)
	if err != nil {
		t.Fatal(err)
	}
	manager := credential.NewManager(backend, nil)
	registry, err := provider.New(provider.Defaults(map[string][]provider.Model{"openai": {{ID: "explicit-test-model"}}})...)
	if err != nil {
		t.Fatal(err)
	}
	selection := provider.Selection{Version: 1, Provider: "openai", Model: provider.Model{ID: "explicit-test-model"}, Endpoint: server.URL, Auth: credential.Reference{Provider: "openai", ID: "work", Method: credential.APIKey}, MaxAttempts: 1, Source: "integration test"}
	client, _, err := registry.Build(provider.BuildConfig{Selection: selection, Resolver: manager})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Respond(t.Context(), llm.Request{Model: llm.Model{ID: "explicit-test-model"}}, llm.RequestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Output) != 1 || result.Output[0].Type != llm.ItemToolCall || requests.Load() != 1 {
		t.Fatal("tool call ownership changed")
	}
	output.Reset()
	if err := Run(t.Context(), []string{"list", "--store", directory}, nil, &output); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), secret) {
		t.Fatal("listing leaked credential")
	}
	output.Reset()
	if err := Run(t.Context(), []string{"logout", "--store", directory, "--provider", "openai", "--id", "work"}, nil, &output); err != nil {
		t.Fatal(err)
	}
	_, err = client.Respond(t.Context(), llm.Request{Model: llm.Model{ID: "explicit-test-model"}}, llm.RequestOptions{})
	if !credential.IsCode(err, "not_found") || requests.Load() != 1 {
		t.Fatal("logout silently fell back")
	}
}
func TestUnsupportedMethodsRedactionCancellationAndAnthropicConfiguration(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "credentials")
	secret := rand.Text()
	for _, p := range []string{"anthropic", "openai-codex", "github-copilot"} {
		var out bytes.Buffer
		err := Run(t.Context(), []string{"login", "--store", dir, "--provider", p, "--id", "work", "--method", "oauth", "--key-stdin"}, strings.NewReader(secret), &out)
		if !credential.IsCode(err, "unsupported_auth_flow") || strings.Contains(out.String(), secret) || strings.Contains(err.Error(), secret) {
			t.Fatal("unsupported flow did not fail safely", err)
		}
	}
	var out bytes.Buffer
	if err := Run(t.Context(), []string{"login", "--store", dir, "--provider", "anthropic", "--id", "work", "--key-stdin"}, strings.NewReader(secret), &out); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := Run(ctx, []string{"login", "--store", dir, "--provider", "openai", "--id", "work", "--key-stdin"}, strings.NewReader(secret), &out)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	out.Reset()
	if err := Run(t.Context(), []string{"methods"}, nil, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "deferred") {
		t.Fatal("support matrix missing")
	}
}
func TestEntitlementDenialAndPolicyTransport(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "credentials")
	secret := rand.Text()
	var out bytes.Buffer
	if err := Run(t.Context(), []string{"login", "--store", dir, "--provider", "openai", "--id", "work", "--key-stdin"}, strings.NewReader(secret), &out); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(403)
		fmt.Fprintf(w, `{"error":{"message":"%s"}}`, secret)
	}))
	defer server.Close()
	store, _ := credential.OpenLocal(dir)
	registry, _ := provider.New(provider.Defaults(map[string][]provider.Model{"openai": {{ID: "model"}}})...)
	config := provider.BuildConfig{Selection: provider.Selection{Version: 1, Provider: "openai", Model: provider.Model{ID: "model"}, Endpoint: server.URL, Auth: credential.Reference{Provider: "openai", ID: "work", Method: credential.APIKey}, MaxAttempts: 1, Source: "test"}, Resolver: credential.NewManager(store, nil)}
	client, _, err := registry.Build(config)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Respond(t.Context(), llm.Request{Model: llm.Model{ID: "model"}}, llm.RequestOptions{})
	var normalized *provider.Error
	if !errors.As(err, &normalized) || normalized.Code != "forbidden" || strings.Contains(err.Error(), secret) {
		t.Fatal("entitlement error leaked", err)
	}
	var denied atomic.Int64
	config.HTTPClient = &http.Client{Transport: denyTransport{&denied}}
	client, _, _ = registry.Build(config)
	_, err = client.Respond(t.Context(), llm.Request{Model: llm.Model{ID: "model"}}, llm.RequestOptions{})
	if err == nil || denied.Load() != 1 {
		t.Fatal("injected enforcement bypassed")
	}
}

type denyTransport struct{ calls *atomic.Int64 }

func (d denyTransport) RoundTrip(*http.Request) (*http.Response, error) {
	d.calls.Add(1)
	return nil, errors.New("denied")
}

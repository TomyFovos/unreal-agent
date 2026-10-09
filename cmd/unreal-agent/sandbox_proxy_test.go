//go:build linux || darwin

package main

import (
	"encoding/json/v2"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/openaicodex"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/internal/testcodex"
)

func TestDockerSandboxForwardProxyHostCanonicalResults(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		code   string
	}{
		{"success", http.StatusOK, ""},
		{"unauthorized", http.StatusUnauthorized, "provider: unauthenticated"},
		{"forbidden", http.StatusForbidden, "provider: forbidden"},
		{"missing-proxy", http.StatusOK, "provider: proxy_unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("SBX_CRED_OPENAI_MODE", "oauth")
			t.Setenv("HTTPS_PROXY", testcodex.DockerProxyURL)
			t.Setenv("https_proxy", "")
			if test.name == "missing-proxy" {
				t.Setenv("HTTPS_PROXY", "")
			}
			auth := sandboxCodexFixture(t)
			before, err := os.ReadFile(auth)
			if err != nil {
				t.Fatal(err)
			}
			info, _ := os.Stat(auth)
			proxy := testcodex.NewOAuthProxy(t, openaicodex.SandboxAccessSentinel)
			proxy.Status.Store(int32(test.status))
			cfg := codexServeConfiguration(t.TempDir(), openaicodex.BaseURL)
			cfg.Runtime.Provider.Auth = credential.Reference{Provider: "openai-codex", ID: "docker-sandbox-oauth", Method: credential.DockerSandboxOAuth}
			cfg.Runtime.ReasoningEffort = llm.ReasoningEffortMedium
			registry, err := registeredProviders(cfg.Runtime, nil)
			if err != nil {
				t.Fatal(err)
			}
			store := filepath.Join(t.TempDir(), "sessions")
			factory, identity, err := agentrunner.NewRuntimeFactory(agentrunner.RuntimeConfig{Identity: cfg.Runtime, SessionDirectory: store, Providers: registry, Credentials: withExternalCodexCredentials(nil, auth), HTTPClient: &http.Client{Transport: proxy.Transport}})
			if err != nil {
				t.Fatal(err)
			}
			policy, err := permission.New(permission.Config{Tools: []string{"read", "grep", "glob"}, ReadRoots: []string{cfg.Runtime.Workspace}, NetworkOrigins: []string{"https://chatgpt.com"}})
			if err != nil {
				t.Fatal(err)
			}
			defer policy.Close()
			owner, err := host.New(t.Context(), host.Config{Directory: store, Build: factory})
			if err != nil {
				t.Fatal(err)
			}
			defer owner.Close()
			options := host.Options{ID: "proxy-fixture", Lifecycle: "interactive", Policy: policy, Workspace: cfg.Runtime.Workspace, Configuration: identity}
			s, err := owner.Create(t.Context(), options)
			if err != nil {
				t.Fatal(err)
			}
			input := inbox.Input{ID: "single-proxy-input", Kind: inbox.InputExternal, Payload: []byte(`"synthetic request"`)}
			if _, err = s.Submit(t.Context(), s.Generation, input); err != nil {
				t.Fatal(err)
			}
			var view host.View
			waitInteractive(t, func() bool {
				view, err = s.Inspect(0, 256)
				if err != nil {
					t.Fatal(err)
				}
				return responseCount(view) == 1 || !view.Running
			})
			if len(view.Operations) != 0 {
				t.Fatal("inference invented a canonical Operation")
			}
			if test.code == "" {
				if responseCount(view) != 1 || !proxy.RequestOK.Load() {
					t.Fatal("Host did not persist the proxy Final")
				}
				// The same submitted input remains idempotent across Host restart.
				if err = owner.Close(); err != nil {
					t.Fatal(err)
				}
				replacement, err := host.New(t.Context(), host.Config{Directory: store, Build: factory})
				if err != nil {
					t.Fatal(err)
				}
				defer replacement.Close()
				s, err = replacement.Resume(t.Context(), options)
				if err != nil {
					t.Fatal("proxy Session failed to resume")
				}
				if _, err = s.Submit(t.Context(), s.Generation, input); err != nil {
					t.Fatal("same input was not retained")
				}
				view, err = s.Inspect(0, 256)
				if err != nil || responseCount(view) != 1 || len(view.Operations) != 0 {
					t.Fatal("resume lost or duplicated history")
				}
			} else if responseCount(view) != 0 || !strings.Contains(view.Failure, test.code) {
				t.Fatal("Host denial was not safe and typed")
			}
			encoded, err := json.Marshal(view)
			if err != nil {
				t.Fatal(err)
			}
			assertNoCredentialLeak(t, string(encoded), openaicodex.SandboxAccessSentinel, "proxy-managed", "private-fixture-response", "private-fixture-code")
			assertNoStoredCredentials(t, store, openaicodex.SandboxAccessSentinel, "proxy-managed", "private-fixture-response", "private-fixture-code")
			expected := int32(1)
			if test.code == "provider: proxy_unavailable" {
				expected = 0
			}
			if proxy.Requests.Load() != expected || proxy.DirectRequests.Load() != 0 {
				t.Fatal("Host retried or used direct egress")
			}
			after, _ := os.ReadFile(auth)
			current, _ := os.Stat(auth)
			if string(after) != string(before) || !current.ModTime().Equal(info.ModTime()) {
				t.Fatal("Host altered existing kit authentication")
			}
		})
	}
}

func TestDockerSandboxForwardProxyChildEnvironmentIsClosed(t *testing.T) {
	t.Setenv("SBX_CRED_OPENAI_MODE", "oauth")
	t.Setenv("HTTPS_PROXY", testcodex.DockerProxyURL)
	t.Setenv("https_proxy", "")
	t.Setenv("HTTP_PROXY", "http://private-fixture:secret@untrusted.invalid:3128")
	t.Setenv("NO_PROXY", "*")
	cfg := agentrunner.RuntimeConfig{}
	cfg.Identity.Provider.Auth = credential.Reference{Provider: "openai-codex", ID: "docker-sandbox-oauth", Method: credential.DockerSandboxOAuth}
	env := childEnvironment(cfg)
	if !slices.Contains(env, "HTTPS_PROXY="+testcodex.DockerProxyURL) || strings.Contains(strings.Join(env, "\n"), "private-fixture") || strings.Contains(strings.Join(env, "\n"), "NO_PROXY") {
		t.Fatal("child relay missing or parent secrets crossed")
	}
	t.Setenv("HTTPS_PROXY", "http://private-fixture:secret@untrusted.invalid:3128")
	if strings.Contains(strings.Join(childEnvironment(cfg), "\n"), "PROXY=") {
		t.Fatal("invalid relay crossed to child")
	}
	t.Setenv("HTTPS_PROXY", testcodex.DockerProxyURL)
	cfg.Identity.Provider.Auth = codexReference()
	if strings.Contains(strings.Join(childEnvironment(cfg), "\n"), "PROXY=") {
		t.Fatal("native child proxy contract changed")
	}
}

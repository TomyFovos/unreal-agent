package openaicodex

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/responsesapi"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/internal/testcodex"
)

func TestDockerSandboxForwardProxyInference(t *testing.T) {
	t.Setenv("HTTPS_PROXY", testcodex.DockerProxyURL)
	t.Setenv("https_proxy", "")
	t.Setenv("NO_PROXY", "*") // must not bypass the mandatory OAuth relay
	proxy := testcodex.NewOAuthProxy(t, SandboxAccessSentinel)
	proxy.Transport.ProxyConnectHeader = http.Header{"Proxy-Authorization": {"private-fixture-proxy-secret"}}
	attempts := 1 // Match the Pilot's explicit per-request retry configuration.
	client, err := NewClient(Config{DockerSandboxOAuth: true, MaxAttempts: &attempts, HTTPClient: &http.Client{Transport: proxy.Transport}})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	policy, err := permission.New(permission.Config{NetworkOrigins: []string{"https://chatgpt.com"}})
	if err != nil {
		t.Fatal(err)
	}
	defer policy.Close()
	request := llm.Request{Model: llm.Model{ID: "gpt-6.1-sol", ReasoningEffort: llm.ReasoningEffortMedium}, Input: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "synthetic request"}}}}
	response, err := client.Respond(permission.WithPolicy(t.Context(), policy), request, llm.RequestOptions{})
	if err != nil || response.Stop != llm.StopComplete || proxy.Connects.Load() != 1 || proxy.Requests.Load() != 1 || proxy.DirectRequests.Load() != 0 || !proxy.RequestOK.Load() {
		t.Fatalf("forward OAuth route failed: successful=%v connects=%d requests=%d direct=%d metadata_ok=%v", err == nil, proxy.Connects.Load(), proxy.Requests.Load(), proxy.DirectRequests.Load(), proxy.RequestOK.Load())
	}
	if proxy.Transport.ProxyConnectHeader.Get("Proxy-Authorization") != "private-fixture-proxy-secret" {
		t.Fatal("injected host transport was mutated")
	}
	if _, err = client.Respond(permission.WithPolicy(t.Context(), permission.DenyAll()), request, llm.RequestOptions{}); permission.Failure(err) == nil || proxy.Connects.Load() != 1 {
		t.Fatal("Permission denial reached the relay")
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = client.Respond(canceled, request, llm.RequestOptions{}); !errors.Is(err, context.Canceled) || proxy.Connects.Load() != 1 {
		t.Fatal("canceled generation reached the relay")
	}
}

func TestDockerSandboxProxyEnvironmentIsClosed(t *testing.T) {
	for _, test := range []struct {
		name, upper, lower string
		valid              bool
	}{
		{"host", testcodex.DockerProxyURL, "", true},
		{"gateway", "http://gateway.docker.internal:3128", "", true},
		{"lowercase", "", testcodex.DockerProxyURL, true},
		{"same", testcodex.DockerProxyURL, testcodex.DockerProxyURL, true},
		{"missing", "", "", false},
		{"conflict", testcodex.DockerProxyURL, "http://gateway.docker.internal:3128", false},
		{"userinfo", "http://private-fixture:secret@host.docker.internal:3128", "", false},
		{"remote", "http://private-fixture.invalid:3128", "", false},
		{"loopback", "http://127.0.0.1:3128", "", false},
		{"ip", "http://192.168.65.1:3128", "", false},
		{"port", "http://host.docker.internal:3129", "", false},
		{"tls", "https://host.docker.internal:3128", "", false},
		{"socks", "socks5://host.docker.internal:3128", "", false},
		{"path", testcodex.DockerProxyURL + "/", "", false},
		{"query", testcodex.DockerProxyURL + "?private-fixture=secret", "", false},
		{"fragment", testcodex.DockerProxyURL + "#private-fixture", "", false},
		{"case", "http://HOST.docker.internal:3128", "", false},
		{"suffix", "http://host.docker.internal.private-fixture:3128", "", false},
		{"control", testcodex.DockerProxyURL + "\nprivate-fixture", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			proxy, err := DockerSandboxProxyURL(func(key string) string {
				if key == "HTTPS_PROXY" {
					return test.upper
				}
				if key == "https_proxy" {
					return test.lower
				}
				return "http://private-fixture:secret@untrusted.invalid:3128"
			})
			if (err == nil) != test.valid || (proxy != nil) != test.valid {
				t.Fatal("unexpected proxy validation outcome")
			}
			if err != nil {
				var typed *SandboxProxyError
				if !errors.As(err, &typed) || strings.Contains(err.Error(), "private-fixture") || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "http:") {
					t.Fatal("proxy failure is not safe and typed")
				}
			}
		})
	}
}

func TestDockerSandboxProxyMissingAndFailureNeverGoDirect(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("https_proxy", "")
	t.Setenv("ALL_PROXY", testcodex.DockerProxyURL)
	proxy := testcodex.NewOAuthProxy(t, SandboxAccessSentinel)
	_, err := NewClient(Config{DockerSandboxOAuth: true, HTTPClient: &http.Client{Transport: proxy.Transport}})
	var typed *SandboxProxyError
	if !errors.As(err, &typed) || proxy.Connects.Load() != 0 || proxy.DirectRequests.Load() != 0 {
		t.Fatal("missing forward route used another path")
	}
	t.Setenv("HTTPS_PROXY", testcodex.DockerProxyURL)
	var direct bool
	proxy.Transport.DialContext = func(_ context.Context, _, address string) (net.Conn, error) {
		if address != "host.docker.internal:3128" {
			direct = true
		}
		return nil, errors.New("synthetic relay unavailable")
	}
	attempts := 1
	client, err := NewClient(Config{DockerSandboxOAuth: true, MaxAttempts: &attempts, HTTPClient: &http.Client{Transport: proxy.Transport}})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err = client.Respond(t.Context(), llm.Request{}, llm.RequestOptions{}); err == nil || direct || proxy.DirectRequests.Load() != 0 {
		t.Fatal("relay failure fell back to direct egress")
	}
}

func TestDockerSandboxProxyHTTPDenials(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Setenv("HTTPS_PROXY", testcodex.DockerProxyURL)
			t.Setenv("https_proxy", "")
			proxy := testcodex.NewOAuthProxy(t, SandboxAccessSentinel)
			proxy.Status.Store(int32(status))
			client, err := NewClient(Config{DockerSandboxOAuth: true, HTTPClient: &http.Client{Transport: proxy.Transport}})
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			_, err = client.Respond(t.Context(), llm.Request{Model: llm.Model{ID: "gpt-6.1-sol", ReasoningEffort: llm.ReasoningEffortMedium}}, llm.RequestOptions{})
			var failure *responsesapi.APIError
			if !errors.As(err, &failure) || failure.StatusCode != status || proxy.Requests.Load() != 1 || proxy.DirectRequests.Load() != 0 {
				t.Fatal("HTTP denial retried, fell back, or lost its status")
			}
		})
	}
}

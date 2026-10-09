package provider

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/openaicodex"
	"github.com/unreallabsai/unreal-agent/harness/llm/responsesapi"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/primitives"
	"github.com/unreallabsai/unreal-agent/internal/testcodex"
)

func TestRemoteFailureSafeStatusAndStage(t *testing.T) {
	for _, test := range []struct {
		status      int
		code, stage string
	}{
		{401, "unauthenticated", "http_status"},
		{403, "forbidden", "http_status"},
		{502, "remote_failure", "http_status"},
		{200, "remote_failure", "stream_error"},
	} {
		failure := normalize(&responsesapi.APIError{StatusCode: test.status, Code: "private-fixture-code", Message: "private-fixture-body", Param: "private-fixture-param", Type: "private-fixture-type"})
		var typed *Error
		if !errors.As(failure, &typed) || typed.Code != test.code || typed.Status != test.status {
			t.Fatalf("typed failure lost: %v", failure)
		}
		for _, safe := range []string{"stage=" + test.stage, "reason=unknown", "retryable="} {
			if !strings.Contains(failure.Error(), safe) {
				t.Fatalf("missing safe diagnostic %q: %v", safe, failure)
			}
		}
		if strings.Contains(failure.Error(), "private-fixture") {
			t.Fatal("provider body leaked")
		}
	}
}

func TestDockerSandboxForwardProxyTransportDiagnostics(t *testing.T) {
	for _, reason := range []string{"timeout", "connection_error"} {
		t.Run(reason, func(t *testing.T) {
			t.Setenv("HTTPS_PROXY", testcodex.DockerProxyURL)
			t.Setenv("https_proxy", "")
			release := make(chan struct{})
			defer close(release)
			proxy := testcodex.NewOAuthProxyWithHandler(t, openaicodex.SandboxAccessSentinel, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if reason == "timeout" {
					select {
					case <-r.Context().Done():
					case <-release:
					}
					return
				}
				connection, _, err := http.NewResponseController(w).Hijack()
				if err != nil {
					t.Error("synthetic TLS connection could not close")
					return
				}
				connection.Close()
			}))
			proxy.Transport.ResponseHeaderTimeout = 50 * time.Millisecond
			registry, err := New(Defaults(map[string][]Model{"openai-codex": {{ID: "gpt-6.1-sol"}}})...)
			if err != nil {
				t.Fatal(err)
			}
			ref := credential.Reference{Provider: "openai-codex", ID: "docker-sandbox-oauth", Method: credential.DockerSandboxOAuth}
			selection := Selection{Version: 1, Provider: "openai-codex", Model: Model{ID: "gpt-6.1-sol"}, Auth: ref, MaxAttempts: 1, Source: "synthetic proxy fixture"}
			client, _, err := registry.Build(BuildConfig{Selection: selection, Resolver: DockerSandboxCodex(sandboxSource(t)), HTTPClient: &http.Client{Transport: proxy.Transport}})
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			policy, err := permission.New(permission.Config{NetworkOrigins: []string{"https://chatgpt.com"}})
			if err != nil {
				t.Fatal(err)
			}
			defer policy.Close()
			_, err = client.Respond(permission.WithPolicy(t.Context(), policy), llm.Request{Model: llm.Model{ID: "gpt-6.1-sol", ReasoningEffort: llm.ReasoningEffortMedium}}, llm.RequestOptions{})
			var failure *Error
			if !errors.As(err, &failure) || failure.Code != "request_failed" || failure.Diagnostic == nil || failure.Diagnostic.Stage != "transport" || failure.Diagnostic.Reason != reason || !failure.Diagnostic.Retryable || failure.Status != 0 {
				t.Fatalf("wrong closed forward-proxy failure: %v", err)
			}
			if proxy.Requests.Load() != 1 || proxy.DirectRequests.Load() != 0 || !proxy.RequestOK.Load() {
				t.Fatal("transport failure retried/fell back or changed auth/routing")
			}
			data, _ := json.Marshal(failure)
			for _, withheld := range []string{openaicodex.SandboxAccessSentinel, "chatgpt.com", "host.docker.internal", "Authorization", "auth.json"} {
				if strings.Contains(string(data)+failure.Error(), withheld) {
					t.Fatal("proxy metadata leaked")
				}
			}
		})
	}
}

func TestRemoteDiagnosticsNeverRetainRawCause(t *testing.T) {
	for _, api := range []*responsesapi.APIError{
		{StatusCode: 502, Code: "server_error", Message: "private-body"},
		{StatusCode: 200, Code: "context_length_exceeded", Message: "private-body", Param: "private-param"},
		{StatusCode: 200, Code: "private-secret-\x1b]52;", Message: "private-body", Type: "private-type"},
	} {
		err := normalize(fmt.Errorf("private-wrapper: %w", api))
		data, _ := json.Marshal(err)
		if strings.Contains(string(data), "private") || strings.Contains(err.Error(), "private") || errors.Unwrap(err) != nil {
			t.Fatal("public failure retained provider payload/cause")
		}
	}
	for _, test := range []struct {
		err    error
		reason string
	}{
		{&net.DNSError{Name: "private-host", Err: "private-body", IsTimeout: true}, "timeout"},
		{&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("private-body")}, "connection_error"},
	} {
		remote := primitives.NewRemoteClientWithHTTPClient(&http.Client{Transport: proxyTestTransport(func(*http.Request) (*http.Response, error) { return nil, test.err })})
		client, err := responsesapi.NewAdapter(remote, responsesapi.Config{Endpoint: "https://private-host.invalid/private-path", MaxAttempts: new(1)})
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.Respond(t.Context(), llm.Request{Model: llm.Model{ID: "fixture"}}, llm.RequestOptions{})
		remote.Close()
		safe := normalize(err)
		var typed *Error
		if !errors.As(safe, &typed) || typed.Code != "request_failed" || typed.Diagnostic == nil || typed.Diagnostic.Stage != "transport" || typed.Diagnostic.Reason != test.reason {
			t.Fatalf("transport not safely typed: %v", safe)
		}
		data, _ := json.Marshal(safe)
		if strings.Contains(string(data), "private") || strings.Contains(safe.Error(), "private") || errors.Unwrap(safe) != nil {
			t.Fatal("transport payload leaked")
		}
	}
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		if normalize(fmt.Errorf("private-context: %w", cause)) != cause {
			t.Fatal("cancellation semantics changed or raw context wrapper retained")
		}
	}
}

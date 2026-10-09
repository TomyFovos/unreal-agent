package responsesapi

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/primitives"
)

func TestResponseFailureDiagnosticsAtActualExchangeBoundary(t *testing.T) {
	for _, test := range []struct {
		name, body, stage, reason string
		status                    int
		retry                     bool
	}{
		{"401", `{"error":{"code":"private-code","message":"private-body"}}`, "http_status", "unknown", 401, false},
		{"403", `{"error":{"type":"permission_error","message":"private-body"}}`, "http_status", "permission_error", 403, false},
		{"429", `{"error":{"code":"rate_limit_exceeded","message":"private-body"}}`, "http_status", "rate_limit_exceeded", 429, true},
		{"502", `{"error":{"code":"server_error","message":"private-body"}}`, "http_status", "server_error", 502, true},
		{"sse-error", "data: {\"type\":\"error\",\"code\":\"server_error\",\"message\":\"private-body\"}\n\n", "stream_error", "server_error", 200, true},
		{"sse-nested-error", "data: {\"type\":\"error\",\"error\":{\"code\":\"context_length_exceeded\",\"message\":\"private-body\"}}\n\n", "stream_error", "context_length_exceeded", 200, false},
		{"sse-unknown-error", "data: {\"type\":\"error\",\"code\":\"private-code\",\"message\":\"private-body\"}\n\n", "stream_error", "unknown", 200, true},
		{"invalid-stream", "data: {private-body}\n\n", "stream_decode", "invalid_response", 200, false},
		{"incomplete-stream", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"private-body\"}\n\n", "stream_incomplete", "incomplete_stream", 200, true},
		{"invalid-response", "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"private-status\"}}\n\n", "response_decode", "invalid_response", 200, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var count atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				count.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				if test.status == http.StatusOK {
					w.Header().Set("Content-Type", "text/event-stream")
				} else {
					w.Header().Set("Content-Type", "application/json")
				}
				w.WriteHeader(test.status)
				io.WriteString(w, test.body)
			}))
			defer server.Close()
			client := newTestAdapterWithConfig(t, Config{Endpoint: server.URL, MaxAttempts: new(1)})
			_, err := client.Respond(t.Context(), validRequest(), llm.RequestOptions{})
			d, ok := Diagnose(err)
			if !ok || d != (FailureDiagnostic{Stage: test.stage, Status: test.status, Reason: test.reason, Retryable: test.retry}) {
				t.Fatalf("unexpected safe diagnosis: %+v, known=%t", d, ok)
			}
			if count.Load() != 1 {
				t.Fatal("diagnostics initiated another request")
			}
			if strings.Contains(fmt.Sprint(d), "private") || strings.Contains(fmt.Sprint(d), server.URL) {
				t.Fatal("unsafe diagnostic")
			}
			if test.stage == "http_status" || test.stage == "stream_error" {
				var original *APIError
				if !errors.As(err, &original) || original.StatusCode != test.status {
					t.Fatal("legacy internal APIError lost")
				}
			}
		})
	}
}

type diagnosticTransport func(*http.Request) (*http.Response, error)

func (f diagnosticTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type diagnosticTimeout struct{}

func (diagnosticTimeout) Error() string   { return "private-transport-timeout" }
func (diagnosticTimeout) Timeout() bool   { return true }
func (diagnosticTimeout) Temporary() bool { return true }

func TestResponseTransportFailureRetainsClosedTypeBeforeStringConversion(t *testing.T) {
	for _, test := range []struct {
		name, reason string
		cause        error
	}{
		{"timeout", "timeout", diagnosticTimeout{}},
		{"connection", "connection_error", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("private-transport-body")}},
		{"unknown", "unknown", errors.New("private-transport-body timeout server_error")},
	} {
		t.Run(test.name, func(t *testing.T) {
			remote := primitives.NewRemoteClientWithHTTPClient(&http.Client{Transport: diagnosticTransport(func(*http.Request) (*http.Response, error) { return nil, test.cause })})
			defer remote.Close()
			client, err := NewAdapter(remote, Config{Endpoint: "https://fixture.invalid/private-path", MaxAttempts: new(1)})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Respond(t.Context(), validRequest(), llm.RequestOptions{})
			d, ok := Diagnose(err)
			if !ok || d.Stage != "transport" || d.Status != 0 || d.Reason != test.reason || !d.Retryable {
				t.Fatalf("closed transport type lost: %+v", d)
			}
			if strings.Contains(fmt.Sprint(d), "private") {
				t.Fatal("raw transport leaked")
			}
		})
	}
}

func TestFailureDiagnosticRejectsUnrecognizedVocabulary(t *testing.T) {
	d := (FailureDiagnostic{Stage: "private-stage", Reason: "private-reason", Status: 9999}).Safe()
	if d.Stage != "unknown" || d.Reason != "unknown" || d.Status != 0 {
		t.Fatal("unrecognized raw fields retained")
	}
}

package responsesapi

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

// A provider classifier sees every failure with its response headers before
// the shared retry policy, on the HTTP status and in-stream paths alike.
func TestClassifyErrorDecidesRetryAndDiagnostic(t *testing.T) {
	exhausted := func(failure *APIError, headers http.Header) *APIError {
		if headers.Get("X-Allowance") != "exhausted" {
			return failure
		}
		copy := *failure
		copy.Code = "usage_limit_reached"
		return &copy
	}
	for _, path := range []string{"http_status", "stream_error"} {
		t.Run(path, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("X-Allowance", "exhausted")
				if path == "http_status" {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = io.WriteString(w, `{"error":{"message":"slow down","type":"rate_limit"}}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, retryErrorEvent("error", "rate_limit_exceeded", "slow down"))
			}))
			defer server.Close()
			adapter := newTestAdapterWithConfig(t, Config{Endpoint: server.URL, MaxAttempts: new(3), ClassifyError: exhausted})
			_, err := adapter.Respond(t.Context(), llm.Request{Model: llm.Model{ID: "m"}}, llm.RequestOptions{})
			var api *APIError
			if !errors.As(err, &api) || api.Code != "usage_limit_reached" {
				t.Fatal("unclassified failure", err)
			}
			diagnostic, ok := Diagnose(err)
			if !ok || diagnostic.Retryable || diagnostic.Reason != "usage_limit_reached" || diagnostic.Stage != path || calls.Load() != 1 {
				t.Fatal("exhausted allowance retried", diagnostic, calls.Load())
			}
		})
	}
	t.Run("unchanged", func(t *testing.T) {
		var calls, seen atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			if calls.Add(1) == 1 {
				_, _ = io.WriteString(w, retryErrorEvent("error", "rate_limit_exceeded", "try again in 1ms"))
				return
			}
			_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[]}}\n\n")
		}))
		defer server.Close()
		adapter := newTestAdapterWithConfig(t, Config{Endpoint: server.URL, MaxAttempts: new(2), ClassifyError: func(failure *APIError, headers http.Header) *APIError {
			seen.Add(1)
			return failure
		}})
		response, err := adapter.Respond(t.Context(), llm.Request{Model: llm.Model{ID: "m"}}, llm.RequestOptions{})
		// The completed response is not a failure, so the classifier sees only the first attempt.
		if err != nil || response.Stop != llm.StopComplete || calls.Load() != 2 || seen.Load() != 1 {
			t.Fatal("unchanged failure lost the shared retry", err, calls.Load(), seen.Load())
		}
	})
}

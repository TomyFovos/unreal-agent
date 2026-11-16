package opencodego

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/responsesapi"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func reply(status int, header http.Header, body string) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	if header.Get("Content-Type") == "" {
		header.Set("Content-Type", "text/event-stream")
	}
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body))}
}

// Mirrors openaicodex TestCodexRetryableFailureThenSuccess: temporary
// failures, including a 429 within the shared backoff cap, keep Codex's retry.
func TestRetryableFailureThenSuccess(t *testing.T) {
	for _, failure := range []string{"transport", "503", "429", "429_retry_after_cap", "stream"} {
		t.Run(failure, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var calls []time.Time
				var logs bytes.Buffer
				previous := slog.Default()
				slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
				defer slog.SetDefault(previous)
				client, err := NewClient(Config{APIKey: "synthetic-key", HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
					calls = append(calls, time.Now())
					if len(calls) == 4 {
						return reply(200, nil, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"recovered\",\"status\":\"completed\",\"output\":[]}}\n\n"), nil
					}
					switch failure {
					case "transport":
						return nil, errors.New("synthetic connection failure")
					case "503":
						return reply(503, nil, "temporary server failure"), nil
					case "429":
						return reply(429, nil, `{"error":{"code":"rate_limit_exceeded"}}`), nil
					case "429_retry_after_cap":
						return reply(429, http.Header{"Retry-After": {"30"}}, `{"error":{"message":"slow down"}}`), nil
					}
					return reply(200, nil, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n"), nil
				})}})
				if err != nil {
					t.Fatal(err)
				}
				defer client.Close()
				response, err := client.Respond(t.Context(), llm.Request{}, llm.RequestOptions{})
				if err != nil || response.ID != "recovered" || len(calls) != 4 {
					t.Fatalf("response=%+v error=%v calls=%d", response, err, len(calls))
				}
				for i, base := range []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second} {
					delay := calls[i+1].Sub(calls[i])
					if failure == "429_retry_after_cap" {
						base = 30 * time.Second
					}
					if delay < base*4/5 || delay > base {
						t.Fatalf("backoff %d = %v", i, delay)
					}
				}
				if strings.Count(logs.String(), "responses API retry scheduled") != 3 || strings.Contains(logs.String(), "synthetic-key") {
					t.Fatal("missing or unsafe retry log", logs.String())
				}
			})
		})
	}
}

// An exhausted allowance is never retried: a child stops and reports instead
// of retrying until the allowance renews (docs/opencode-go.md).
func TestExhaustedAllowanceIsNotRetried(t *testing.T) {
	for name, failure := range map[string]func(now time.Time) *http.Response{
		"retry_after_seconds": func(time.Time) *http.Response {
			return reply(429, http.Header{"Retry-After": {"3600"}, "Content-Type": {"application/json"}}, `{"error":{"message":"limit reached"}}`)
		},
		"retry_after_date": func(now time.Time) *http.Response {
			return reply(429, http.Header{"Retry-After": {now.Add(5 * time.Hour).UTC().Format(http.TimeFormat)}}, "limit reached")
		},
		"shared_code": func(time.Time) *http.Response {
			return reply(200, nil, "data: {\"type\":\"error\",\"code\":\"insufficient_quota\",\"message\":\"quota\"}\n\n")
		},
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				client, err := NewClient(Config{APIKey: "synthetic-key", HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
					calls++
					return failure(time.Now()), nil
				})}})
				if err != nil {
					t.Fatal(err)
				}
				defer client.Close()
				_, err = client.Respond(t.Context(), llm.Request{}, llm.RequestOptions{})
				diagnostic, ok := responsesapi.Diagnose(err)
				if !ok || diagnostic.Retryable || calls != 1 {
					t.Fatalf("error=%v diagnostic=%+v calls=%d", err, diagnostic, calls)
				}
				if want := map[bool]string{true: "insufficient_quota", false: "usage_limit_reached"}[name == "shared_code"]; diagnostic.Reason != want {
					t.Fatal("reason", diagnostic.Reason)
				}
			})
		})
	}
}

func TestClassifyErrorKeepsOtherFailures(t *testing.T) {
	now := time.Now()
	for _, c := range []struct {
		status int
		header string
		code   string
	}{
		{429, "", ""}, {429, "30", ""}, {429, "soon", ""}, {429, now.Add(-time.Hour).UTC().Format(http.TimeFormat), ""},
		{503, "3600", ""}, {200, "3600", ""}, {429, "31", "usage_limit_reached"},
	} {
		failure := &responsesapi.APIError{StatusCode: c.status, Code: "original"}
		got := classifyError(failure, http.Header{"Retry-After": {c.header}})
		if want := map[bool]string{true: c.code, false: "original"}[c.code != ""]; got.Code != want || failure.Code != "original" {
			t.Fatal(c, got.Code, failure.Code)
		}
	}
}

// Mirrors openaicodex TestCodexRetryCancellationStopsBackoff.
func TestRetryCancellationStopsBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		var calls int
		client, err := NewClient(Config{APIKey: "synthetic-key", HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			return nil, errors.New("connection failed")
		})}})
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		done := make(chan error, 1)
		go func() { _, err := client.Respond(ctx, llm.Request{}, llm.RequestOptions{}); done <- err }()
		synctest.Wait()
		started := time.Now()
		cancel()
		synctest.Wait()
		if err := <-done; !errors.Is(err, context.Canceled) || calls != 1 || time.Since(started) != 0 {
			t.Fatalf("error=%v calls=%d elapsed=%v", err, calls, time.Since(started))
		}
	})
}

// Mirrors openaicodex TestCodexDefaultRequestAndStreamRetryLimits.
func TestDefaultRequestAndStreamRetryLimits(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "request", true: "stream"}[streaming], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				client, err := NewClient(Config{APIKey: "synthetic-key", HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
					calls++
					if streaming {
						return reply(200, nil, "data: {\"type\":\"response.created\"}\n\n"), nil
					}
					return reply(503, nil, "temporary failure"), nil
				})}})
				if err != nil {
					t.Fatal(err)
				}
				defer client.Close()
				_, err = client.Respond(t.Context(), llm.Request{}, llm.RequestOptions{})
				want := DefaultMaxAttempts
				if streaming {
					want = DefaultMaxStreamAttempts
				}
				if err == nil || calls != want {
					t.Fatalf("error=%v attempts=%d want=%d", err, calls, want)
				}
			})
		})
	}
}

package openaicodex

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
)

func TestCodexRetryableFailureThenSuccess(t *testing.T) {
	for _, failure := range []string{"transport", "503", "429", "stream"} {
		t.Run(failure, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var calls []time.Time
				var logs bytes.Buffer
				previous := slog.Default()
				slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
				defer slog.SetDefault(previous)
				transport := sandboxTestTransport(func(*http.Request) (*http.Response, error) {
					calls = append(calls, time.Now())
					status, body := 200, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"recovered\",\"status\":\"completed\",\"output\":[]}}\n\n"
					if len(calls) < 4 {
						switch failure {
						case "transport":
							return nil, errors.New("synthetic connection failure")
						case "503":
							status, body = 503, "temporary server failure"
						case "429":
							status, body = 429, `{"error":{"code":"rate_limit_exceeded"}}`
						case "stream":
							body = "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n"
						}
					}
					return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
				})
				client, err := NewClient(Config{AccessToken: "synthetic-token", AccountID: "synthetic-account", HTTPClient: &http.Client{Transport: transport}})
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
					if delay < base*4/5 || delay > base {
						t.Fatalf("backoff %d = %v", i, delay)
					}
				}
				if strings.Count(logs.String(), "responses API retry scheduled") != 3 || strings.Contains(logs.String(), "synthetic-token") {
					t.Fatal("missing or unsafe retry log", logs.String())
				}
			})
		})
	}
}

func TestCodexRetryCancellationStopsBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		var calls int
		client, err := NewClient(Config{AccessToken: "synthetic-token", AccountID: "synthetic-account", HTTPClient: &http.Client{Transport: sandboxTestTransport(func(*http.Request) (*http.Response, error) {
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

func TestCodexDefaultRequestAndStreamRetryLimits(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "request", true: "stream"}[streaming], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				client, err := NewClient(Config{AccessToken: "synthetic-token", AccountID: "synthetic-account", HTTPClient: &http.Client{Transport: sandboxTestTransport(func(*http.Request) (*http.Response, error) {
					calls++
					status, body := 503, "temporary failure"
					if streaming {
						status, body = 200, "data: {\"type\":\"response.created\"}\n\n"
					}
					return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
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

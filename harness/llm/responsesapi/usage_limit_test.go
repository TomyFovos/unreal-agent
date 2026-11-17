package responsesapi

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/primitives"
)

func TestUsageLimitTypeAndResetHintsAtExchangeBoundary(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now()
		until := now.Add(43 * time.Hour)
		for _, tc := range []struct {
			name, body, header string
			reset              time.Time
			status             int
		}{
			{"subscription-type", fmt.Sprintf(`{"error":{"type":"usage_limit_reached","message":"private","resets_at":%d}}`, until.Unix()), "", until, 429},
			{"code", fmt.Sprintf(`{"error":{"code":"usage_limit_reached","resets_at":"%s"}}`, until.Format(time.RFC3339)), "", until, 429},
			{"relative-body", `{"error":{"type":"usage_limit_reached","resets_in_seconds":154800}}`, "", until, 429},
			{"retry-after", `{"error":{"type":"usage_limit_reached"}}`, "154800", until, 429},
			{"retry-after-date", `{"error":{"code":"usage_limit_reached"}}`, until.UTC().Format(http.TimeFormat), until, 429},
			{"message", `{"error":{"type":"usage_limit_reached","message":"Try again in 43 hours."}}`, "", until, 429},
			{"no-hint", `{"error":{"type":"usage_limit_reached","message":"private"}}`, "", time.Time{}, 429},
			{"bad-hints", `{"error":{"code":"usage_limit_reached","resets_at":"bad","message":"private"}}`, "bad", time.Time{}, 429},
			{"sse-error", fmt.Sprintf("data: {\"type\":\"error\",\"error\":{\"type\":\"usage_limit_reached\",\"resets_at\":%d}}\n\n", until.Unix()), "", until, 200},
			{"sse-failed", fmt.Sprintf("data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"usage_limit_reached\",\"resets_at\":%d}}}\n\n", until.Unix()), "", until, 200},
		} {
			t.Log(tc.name)
			func() {
				calls := 0
				remote := primitives.NewRemoteClientWithHTTPClient(&http.Client{Transport: diagnosticTransport(func(*http.Request) (*http.Response, error) {
					calls++
					headers := http.Header{"Retry-After": {tc.header}, "Content-Type": {"application/json"}}
					if tc.status == 200 {
						headers.Set("Content-Type", "text/event-stream")
					}
					return &http.Response{StatusCode: tc.status, Header: headers, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
				})})
				defer remote.Close()
				client, err := NewAdapter(remote, Config{Endpoint: "https://fixture.invalid/responses"})
				if err != nil {
					t.Fatal(err)
				}
				_, err = client.Respond(t.Context(), validRequest(), llm.RequestOptions{})
				d, known := Diagnose(err)
				if !known || d.Reason != "usage_limit_reached" || d.Retryable || calls != 1 {
					t.Fatalf("usage limit entered transient retry: diagnostic=%+v calls=%d err=%v", d, calls, err)
				}
				var limit llm.UsageLimitFailure
				if !errors.As(err, &limit) {
					t.Fatal("reset lost", err)
				}
				if got, reached := limit.UsageLimitReset(); !reached || !got.Equal(tc.reset) {
					t.Fatal("reset", got, tc.reset)
				}
			}()
		}
	})
}

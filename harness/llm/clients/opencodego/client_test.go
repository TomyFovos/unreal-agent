package opencodego

import (
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/responsesapi"
)

const completed = "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[]}}\n\n"

// testBaseURL keeps loopback test servers under the OpenCode Go path.
func testBaseURL(server *httptest.Server) string { return server.URL + "/inference/go/openai/v1" }

// Mirrors openaicodex TestClientUsesSubscriptionProtocol and
// TestPromptCacheKeyStableAcrossSessionCalls.
func TestClientUsesGoSubscriptionProtocol(t *testing.T) {
	const session = "unchanging-session"
	want := fmt.Sprintf("%x", sha256.Sum256([]byte(session)))
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		if r.URL.Path != "/inference/go/openai/v1/responses" || r.Method != "POST" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		for key, value := range map[string]string{"Authorization": "Bearer go-key", "Accept": "text/event-stream", "Content-Type": "application/json", "User-Agent": "unreal-agent"} {
			if r.Header.Get(key) != value {
				t.Errorf("incorrect %s header", key)
			}
		}
		if session := r.Header.Get("x-opencode-session"); n <= 4 && session != want || n > 4 && session != "" {
			t.Error("session cache identity changed", n)
		}
		var body map[string]any
		if err := json.UnmarshalRead(r.Body, &body); err != nil {
			t.Error(err)
		}
		if body["model"] != "deepseek-v4.1-flash" || body["stream"] != true || body["store"] != false || body["prompt_cache_key"] != nil {
			t.Errorf("body = %#v", body)
		}
		if input, ok := body["input"].([]any); !ok || input[0].(map[string]any)["role"] != "system" || input[0].(map[string]any)["content"] != "Our own instructions" {
			t.Error("system message was changed")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, completed)
	}))
	defer server.Close()
	client, err := NewClient(Config{APIKey: "go-key", BaseURL: testBaseURL(server)})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	request := llm.Request{Model: llm.Model{ID: "deepseek-v4.1-flash"}, Input: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleSystem, Text: "Our own instructions"}}}}
	for i := range 4 {
		request.Input = append(request.Input, llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: fmt.Sprint(i)}})
		if response, err := client.Respond(t.Context(), request, llm.RequestOptions{CacheKey: session}); err != nil || response.Stop != llm.StopComplete {
			t.Fatal(response, err)
		}
	}
	// Without a cache key, no session header is invented.
	if _, err := client.Respond(t.Context(), request, llm.RequestOptions{}); err != nil || requests.Load() != 5 {
		t.Fatal(err, requests.Load())
	}
}

// Mirrors openaicodex TestClientRejectsRedirects.
func TestClientRejectsRedirects(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	for _, injected := range []*http.Client{nil, {}} {
		client, err := NewClient(Config{APIKey: "go-key", BaseURL: testBaseURL(server), HTTPClient: injected})
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.Respond(t.Context(), llm.Request{}, llm.RequestOptions{})
		client.Close()
		var apiErr *responsesapi.APIError
		if !errors.As(err, &apiErr) || apiErr.StatusCode != 307 || redirected.Load() != 0 {
			t.Fatalf("redirect error = %v, target requests = %d", err, redirected.Load())
		}
	}
}

// Mirrors openaicodex TestClientUnauthorizedDoesNotFallback.
func TestClientUnauthorizedIsNotRetried(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(401)
		_, _ = io.WriteString(w, `{"error":{"message":"unauthorized","code":"invalid_api_key"}}`)
	}))
	defer server.Close()
	client, err := NewClient(Config{APIKey: "go-key", BaseURL: testBaseURL(server)})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_, err = client.Respond(t.Context(), llm.Request{}, llm.RequestOptions{})
	var apiErr *responsesapi.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 401 || requests.Load() != 1 || strings.Contains(err.Error(), "go-key") {
		t.Fatalf("error = %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := client.Respond(ctx, llm.Request{}, llm.RequestOptions{}); !errors.Is(err, context.Canceled) || requests.Load() != 1 {
		t.Fatalf("canceled request = %v", err)
	}
}

// Mirrors openaicodex TestClientConfigurationValidation. Only OpenCode Go is a
// destination; Zen and every other path or host are refused.
func TestClientConfigurationValidation(t *testing.T) {
	for _, url := range []string{"", BaseURL, BaseURL + "/", "http://127.0.0.1:1234/inference/go/openai/v1", "http://[::1]:1234/inference/go"} {
		if _, err := validateBaseURL(url); err != nil {
			t.Errorf("rejected %q: %v", url, err)
		}
	}
	for _, url := range []string{
		"https://opencode.ai/zen/v1", "https://opencode.ai/inference/zen/openai/v1", "https://opencode.ai/inference/go/v1",
		"http://opencode.ai/inference/go/openai/v1", "https://opencode.ai.evil.example/inference/go/openai/v1", "https://api.deepseek.com/v1",
		BaseURL + "?token=bad", BaseURL + "#fragment", "http://localhost:1234/inference/go/openai/v1", "http://user:pass@127.0.0.1:1234/inference/go/openai/v1",
		"http://127.0.0.1:1234", "http://127.0.0.1:1234/zen/v1", "http://127.0.0.1:1234/inference/gox", "http://127.0.0.1:1234/inference/go/../zen",
		"http://127.0.0.1:1234/inference/go%2F..%2Fzen", "file:///inference/go/",
	} {
		if _, err := validateBaseURL(url); err == nil {
			t.Errorf("accepted %q", url)
		}
	}
	for _, config := range []Config{{}, {APIKey: " "}, {APIKey: "go-key", MaxAttempts: new(0)}, {APIKey: "go-key", BaseURL: "https://opencode.ai/zen/v1"}} {
		if client, err := NewClient(config); err == nil || client != nil {
			t.Fatal("invalid configuration accepted", config.BaseURL)
		}
	}
}

// Mirrors openaicodex TestClientNeverSendsUltra. Flash accepts every effort
// up to max, so the others pass through unchanged.
// The model list belongs to the same server as the endpoint: the public one
// for OpenCode Go, a loopback test server's own, and nothing else.
func TestModelsURLFollowsTheEndpoint(t *testing.T) {
	for base, want := range map[string]string{"": ModelsURL, BaseURL: ModelsURL, BaseURL + "/": ModelsURL, "http://127.0.0.1:8123/inference/go/openai/v1": "http://127.0.0.1:8123/inference/go/v1/models"} {
		if got, err := ModelsURLFor(base); err != nil || got != want {
			t.Errorf("%q: %q %v", base, got, err)
		}
	}
	for _, base := range []string{"https://opencode.ai/inference/openai/v1", "http://example.com/inference/go/openai/v1", "http://127.0.0.1:8123/zen/v1"} {
		if got, err := ModelsURLFor(base); err == nil || got != "" {
			t.Errorf("%q accepted: %q", base, got)
		}
	}
}

func TestClientSendsMaxForUltra(t *testing.T) {
	var sent []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Reasoning struct {
				Effort string `json:"effort"`
			} `json:"reasoning"`
		}
		if err := json.UnmarshalRead(r.Body, &body); err != nil {
			t.Error(err)
		}
		sent = append(sent, body.Reasoning.Effort)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, completed)
	}))
	defer server.Close()
	client, err := NewClient(Config{APIKey: "go-key", BaseURL: testBaseURL(server)})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	for _, effort := range []llm.ReasoningEffort{llm.ReasoningEffortLow, llm.ReasoningEffortHigh, llm.ReasoningEffortMax, llm.ReasoningEffortUltra} {
		request := llm.Request{Model: llm.Model{ID: "deepseek-v4.1-flash", ReasoningEffort: effort}, Input: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "hi"}}}}
		if _, err := client.Respond(t.Context(), request, llm.RequestOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(sent, ",") != "low,high,max,max" {
		t.Fatal("sent efforts", sent)
	}
}

// kimi-k2.7-code lists no efforts, so no reasoning object is sent and the
// server's own default stays.
func TestClientSendsNoReasoningForAModelWithoutEfforts(t *testing.T) {
	var bodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.UnmarshalRead(r.Body, &body); err != nil {
			t.Error(err)
		}
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, completed)
	}))
	defer server.Close()
	client, err := NewClient(Config{APIKey: "go-key", BaseURL: testBaseURL(server)})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	request := llm.Request{Model: llm.Model{ID: "kimi-k2.7-code"}, Input: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "hi"}}}}
	if _, err := client.Respond(t.Context(), request, llm.RequestOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 1 {
		t.Fatalf("requests %d", len(bodies))
	}
	if reasoning, ok := bodies[0]["reasoning"]; ok {
		t.Fatal("reasoning was sent", reasoning)
	}
}

// OfferedModels is the Flash-first list the 2026-11-05 and 2026-11-14 surveys
// verified.
func TestOfferedModelsAreTheSurveyedModels(t *testing.T) {
	want := "deepseek-v4.1-flash,deepseek-v4-pro,glm-5.2,glm-5.3,grok-4.7,glm-5.3-flash,gpt-5.6-luna,gpt-6-luna,claude-haiku-5-5,hy3,hy4-preview,kimi-k2.7-code,kimi-k3,mimo-v2.6-pro,minimax-m3,minimax-m3.1-flash,longcat-2.0,step-5-preview,qwen3.8-max,qwen3.8-flash,qwen3.7-plus,kimi-k3.1-pro,gemini-4-argon,hy4"
	if got := OfferedModels(); strings.Join(got, ",") != want || len(got) != 24 {
		t.Fatal("offered models", got)
	}
}

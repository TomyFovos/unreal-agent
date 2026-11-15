package openai

import (
	"encoding/json/v2"
	"errors"
	"github.com/unreallabsai/unreal-agent/harness/llm/responsesapi"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

func TestNewClientRequiresAPIKey(t *testing.T) {
	client, err := NewClient(Config{APIKey: " "})
	if err == nil || err.Error() != "OpenAI API key must be set" || client != nil {
		t.Fatalf("client, error = (%#v, %v)", client, err)
	}
}

func TestNewClientRequiresBaseURL(t *testing.T) {
	client, err := NewClient(Config{APIKey: "test-key", BaseURL: " "})
	if err == nil || err.Error() != "OpenAI base URL must be set" || client != nil {
		t.Fatalf("client, error = (%#v, %v)", client, err)
	}
}

func TestClientCallsResponsesAPI(t *testing.T) {
	const cacheKey = "84097828fc31a8c8d29210df48901a85de7fd013f686b17be77d1be29cb7a98b"
	requestSeen := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/responses" {
			t.Errorf("path = %q", request.URL.Path)
		}
		if authorization := request.Header.Get("Authorization"); authorization != "Bearer test-key" {
			t.Errorf("authorization = %q", authorization)
		}
		if accept := request.Header.Get("Accept"); accept != "text/event-stream" {
			t.Errorf("accept = %q", accept)
		}
		var body struct {
			PromptCacheKey *string `json:"prompt_cache_key"`
			Model          string  `json:"model"`
			Stream         *bool   `json:"stream"`
		}
		if err := json.UnmarshalRead(request.Body, &body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if body.Model != "gpt-test" || (body.Stream == nil || !*body.Stream) {
			t.Errorf("body = %#v", body)
		}
		if body.PromptCacheKey == nil || *body.PromptCacheKey != cacheKey {
			t.Errorf("prompt_cache_key = %v, want %q", body.PromptCacheKey, cacheKey)
		}
		requestSeen <- struct{}{}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("data: {\"type\":\"response.completed\",\"response\":" + `{"id":"resp-1","status":"completed","output":[],"usage":{}}` + "}\n\n"))
	}))
	defer server.Close()

	client, err := NewClient(Config{APIKey: "test-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("close client: %v", err)
		}
	})
	request := llm.Request{
		Model: llm.Model{ID: "gpt-test"},
		Input: []llm.Item{{
			Type: llm.ItemMessage,
			Data: llm.Message{Role: llm.RoleUser, Text: "hello"},
		}},
	}
	response, err := client.Respond(t.Context(), request, llm.RequestOptions{CacheKey: "session-1"})
	if err != nil {
		t.Fatalf("respond: %v", err)
	}
	<-requestSeen
	if response.ID != "resp-1" || response.Stop != llm.StopComplete {
		t.Fatalf("response = %#v", response)
	}
}

func TestExplicitEffortsAreSentUnchangedAndRejectionsExplainSelection(t *testing.T) {
	for _, effort := range []llm.ReasoningEffort{"low", "medium", "high", "xhigh", "max"} {
		t.Run(string(effort), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var body struct {
					Reasoning struct {
						Effort string `json:"effort"`
					} `json:"reasoning"`
				}
				if json.UnmarshalRead(r.Body, &body) != nil || body.Reasoning.Effort != string(effort) {
					t.Error("selected effort changed")
				}
				w.WriteHeader(400)
				w.Write([]byte(`{"error":{"type":"invalid_request_error","code":"unsupported_value","param":"reasoning.effort","message":"effort is not accepted; fake-effort-key"}}`))
			}))
			defer server.Close()
			client, err := NewClient(Config{APIKey: "fake-effort-key", BaseURL: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			_, err = client.Respond(t.Context(), llm.Request{Model: llm.Model{ID: "gpt-test", ReasoningEffort: effort}}, llm.RequestOptions{})
			if err == nil || !strings.Contains(err.Error(), "reasoning.effort="+string(effort)) || !strings.Contains(err.Error(), "unsupported_value") || !strings.Contains(err.Error(), "not accepted") || strings.Contains(err.Error(), "fake-effort-key") || calls != 1 {
				t.Fatalf("rejection or retry changed: %v calls=%d", err, calls)
			}
		})
	}
}

func TestEffortSelectionPreservesContextOverflowClassification(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":{"code":"context_length_exceeded","message":"context window exceeded","param":"input"}}`))
	}))
	defer server.Close()
	client, err := NewClient(Config{APIKey: "fake-key", BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_, err = client.Respond(t.Context(), llm.Request{Model: llm.Model{ID: "gpt-test", ReasoningEffort: "max"}}, llm.RequestOptions{})
	var effort *EffortError
	var api *responsesapi.APIError
	if errors.As(err, &effort) || !errors.As(err, &api) || api.Code != "context_length_exceeded" {
		t.Fatal("effort reporting replaced context overflow", err)
	}
}

package openaicodex

import (
	"crypto/sha256"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

func TestPromptCacheKeyStableAcrossSessionCalls(t *testing.T) {
	const session = "unchanging-session"
	want := fmt.Sprintf("%x", sha256.Sum256([]byte(session)))
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body struct {
			Key string `json:"prompt_cache_key"`
		}
		if err := json.UnmarshalRead(r.Body, &body); err != nil {
			t.Error(err)
		}
		if body.Key != want || r.Header.Get("session-id") != want {
			t.Error("session cache identity changed")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[]}}\n\n")
	}))
	defer server.Close()
	client, err := NewClient(Config{AccessToken: "fixture-token", AccountID: "fixture-account", BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	request := llm.Request{Model: llm.Model{ID: "fixture-model"}}
	for i := range 4 {
		request.Input = append(request.Input, llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: fmt.Sprint(i)}})
		if _, err := client.Respond(t.Context(), request, llm.RequestOptions{CacheKey: session}); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 4 {
		t.Fatal("missing calls", calls)
	}
}

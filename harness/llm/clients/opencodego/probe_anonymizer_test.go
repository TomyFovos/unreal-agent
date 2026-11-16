package opencodego

import (
	"encoding/json/v2"
	"net/http"
	"strings"
	"testing"
)

func TestProbeAnonymizerRemovesSecretsAndIdentifiers(t *testing.T) {
	const key = "sk-test-SECRET-KEY-0123456789"
	const session = "SESSION-VALUE-abcdef"
	const encrypted = "ENCRYPTED-REASONING-BLOB-0123456789abcdef0123456789abcdef"
	raw := rawExchange{
		Method: http.MethodPost,
		Path:   "/inference/go/openai/v1/responses",
		RequestHeaders: http.Header{
			"Authorization":      {"Bearer " + key},
			"X-Opencode-Session": {session},
			"User-Agent":         {"unreal-agent"},
		},
		RequestBody: []byte(`{"model":"deepseek-v4.1-flash","prompt_cache_key":"cache-key-1","input":[{"type":"reasoning","id":"rs_original","encrypted_content":"` + encrypted + `"},{"type":"function_call_output","call_id":"call_original","output":"42"}]}`),
		Status:      http.StatusBadRequest,
		ResponseHeaders: http.Header{
			"Content-Type": {"text/event-stream"},
			"X-Request-Id": {"request-original"},
			"Set-Cookie":   {"session=" + session},
		},
		ResponseBody: []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_original\",\"user\":\"account-original\",\"output\":[{\"type\":\"function_call\",\"call_id\":\"call_original\",\"id\":\"rs_original\"}],\"error\":{\"message\":\"bad key " + key + "\"}}}\n\n"),
		Error:        "echo " + key,
	}
	anonymizer := newProbeAnonymizer(key, session)
	exchange := anonymizer.exchange(raw)
	data, err := json.Marshal(exchange)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, leaked := range []string{key, session, encrypted, "rs_original", "call_original", "resp_original", "account-original", "request-original", "cache-key-1"} {
		if strings.Contains(text, leaked) {
			t.Fatalf("capture still contains %q: %s", leaked, text)
		}
	}
	if _, ok := exchange.RequestHeaders["Authorization"]; ok {
		t.Fatal("Authorization header kept")
	}
	if _, ok := exchange.ResponseHeaders["Set-Cookie"]; ok {
		t.Fatal("Set-Cookie header kept")
	}
	if got := exchange.RequestHeaders["X-Opencode-Session"]; len(got) != 1 || got[0] != "<session>" {
		t.Fatal("session header not replaced", got)
	}
	if got := exchange.RequestHeaders["User-Agent"]; len(got) != 1 || got[0] != "unreal-agent" {
		t.Fatal("non-identifying header changed", got)
	}
	if !strings.Contains(text, "<encrypted up to 64 bytes>") {
		t.Fatal("encrypted reasoning lost its size class", text)
	}
	// The same identifier maps to one placeholder across request and response.
	if strings.Count(text, `"<call-1>"`) != 2 || strings.Count(text, `"<id-1>"`) != 2 {
		t.Fatal("identifiers do not line up", text)
	}
	if len(exchange.Events) != 1 || exchange.Events[0].Event != "response.completed" || exchange.Events[0].Type != "response.completed" {
		t.Fatal("stream events lost", exchange.Events)
	}
}

func TestProbeRecorderRefusesOtherDestinations(t *testing.T) {
	recorder := &probeRecorder{base: http.DefaultTransport}
	for _, target := range []string{
		"https://opencode.ai/inference/openai/v1/responses",
		"https://opencode.ai/zen/v1/responses",
		"http://opencode.ai/inference/go/openai/v1/responses",
		"https://example.com/inference/go/openai/v1/responses",
	} {
		request, err := http.NewRequest(http.MethodPost, target, nil)
		if err != nil {
			t.Fatal(err)
		}
		if response, err := recorder.RoundTrip(request); err == nil || response != nil {
			t.Fatal("probe allowed", target)
		}
	}
	if recorder.count() != 0 {
		t.Fatal("refused requests were sent")
	}
}

func TestFindAllowanceReportsFieldsAndHeaders(t *testing.T) {
	exchanges := []probeExchange{{
		ResponseHeaders: map[string][]string{"X-Opencode-Allowance": {"5h=0.2"}},
		Events:          []probeEvent{{Type: "response.completed", Data: map[string]any{"response": map[string]any{"opencode": map[string]any{"allowance": map[string]any{"remaining": 0.8}}}}}},
	}}
	found := findAllowance(exchanges)
	if len(found) != 2 || !strings.Contains(strings.Join(found, "\n"), `.opencode.allowance = {"remaining":0.8}`) {
		t.Fatal("allowance fields not found", found)
	}
}

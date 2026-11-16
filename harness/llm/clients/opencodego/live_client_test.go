package opencodego

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

// The live client check sends four requests through Client with the stored
// key (docs/opencode-go.md): it confirms the Session header, prompt caching
// and a tool round trip against the real endpoint. It runs only with
// UNREAL_LIVE_OPENCODE_GO=1, retries nothing and writes nothing to disk.
func TestLiveOpenCodeGoClient(t *testing.T) {
	if os.Getenv("UNREAL_LIVE_OPENCODE_GO") != "1" {
		t.Skip("set UNREAL_LIVE_OPENCODE_GO=1 to call OpenCode Go with the stored key")
	}
	key := storedKey(t)
	recorder := &probeRecorder{base: http.DefaultTransport}
	one := 1
	client, err := NewClient(Config{APIKey: key, HTTPClient: &http.Client{Transport: recorder}, MaxAttempts: &one, MaxStreamAttempts: &one})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	session := rand.Text()
	options := llm.RequestOptions{CacheKey: session}
	respond := func(name string, effort llm.ReasoningEffort, input []llm.Item, tools []llm.Tool) llm.Response {
		t.Helper()
		response, err := client.Respond(t.Context(), llm.Request{Model: llm.Model{ID: probeModel, ReasoningEffort: effort}, Input: input, Tools: tools}, options)
		if err != nil {
			t.Fatalf("%s: %s", name, strings.ReplaceAll(err.Error(), key, "<key>"))
		}
		var types []string
		for _, item := range response.Output {
			types = append(types, string(item.Type))
		}
		t.Logf("%s (%s): output %v, input %d, cached %d, output %d tokens", name, effort, types, response.Usage.InputTokens, response.Usage.CachedInputTokens, response.Usage.OutputTokens)
		return response
	}

	prefix := []llm.Item{message(llm.RoleSystem, strings.Repeat("This sentence pads the shared system prompt so that the cached prefix is long enough to measure. ", 120)+"Answer tersely.")}
	plain := append(slices.Clone(prefix), message(llm.RoleUser, "Reply with the single word: ok"))
	respond("plain", llm.ReasoningEffortLow, plain, nil)
	if cached := respond("plain_again", llm.ReasoningEffortLow, plain, nil); cached.Usage.CachedInputTokens == 0 {
		t.Error("the repeated prefix was not read from the cache")
	}

	tools := []llm.Tool{{Type: llm.ToolFunction, Name: "lookup_number", Description: "Return the number stored under a key.", Parameters: map[string]any{
		"type":                 "object",
		"properties":           map[string]any{"key": map[string]any{"type": "string"}},
		"required":             []any{"key"},
		"additionalProperties": false,
	}}}
	input := append(slices.Clone(prefix), message(llm.RoleUser, "Call lookup_number with key alpha, then reply with only the number it returns."))
	called := respond("tool_call", llm.ReasoningEffortHigh, input, tools)
	index := slices.IndexFunc(called.Output, func(item llm.Item) bool { return item.Type == llm.ItemToolCall })
	if index < 0 {
		t.Fatal("tool_call: the model did not call the tool")
	}
	call := called.Output[index].Data.(llm.ToolCall)
	input = append(input, called.Output...)
	input = append(input, llm.Item{Type: llm.ItemToolResult, Data: llm.ToolResult{CallID: call.CallID, Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "42"}}}})
	answered := respond("tool_result", llm.ReasoningEffortHigh, input, tools)
	text := ""
	for _, item := range answered.Output {
		if m, ok := item.Data.(llm.Message); ok {
			text += m.Text
		}
	}
	if !strings.Contains(text, "42") {
		t.Errorf("tool_result: answer %q does not use the tool result", text)
	}

	want := fmt.Sprintf("%x", sha256.Sum256([]byte(session)))
	exchanges := recorder.since(0)
	if len(exchanges) != 4 {
		t.Fatal("requests sent", len(exchanges))
	}
	for i, exchange := range exchanges {
		var body map[string]any
		if err := json.Unmarshal(exchange.RequestBody, &body); err != nil {
			t.Fatal(err)
		}
		if exchange.Path != "/inference/go/openai/v1/responses" || exchange.RequestHeaders.Get("x-opencode-session") != want || exchange.RequestHeaders.Get("User-Agent") != "unreal-agent" || body["prompt_cache_key"] != nil {
			t.Error("request", i, "did not use the Session header protocol")
		}
		if exchange.Status != http.StatusOK || exchange.Error != "" {
			t.Error("request", i, exchange.Status, errors.New(strings.ReplaceAll(exchange.Error, key, "<key>")))
		}
	}
}

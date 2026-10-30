package host

import (
	"bytes"
	"encoding/json/v2"
	"reflect"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func TestObservationRetainsMetadataRecentHistoryAndCanonicalBodies(t *testing.T) {
	s := &Session{ID: "s", snapshot: sessionstore.Snapshot{Session: session.Session{ID: "s"}}}
	body := strings.Repeat("a", 159) + "👨‍👩‍👧‍👦" + strings.Repeat("private old body ", 2000)
	items := []sessionstore.Item{
		{Kind: sessionstore.ItemInput, Data: input("old", body)},
		{Kind: sessionstore.ItemModelResponse, Data: sessionstore.ModelResponse{TurnID: "turn", Response: llm.Response{ID: "response", Model: "model", Usage: llm.Usage{InputTokens: 12, OutputTokens: 3}, Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: body}}, {Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "call", Name: "Read", Arguments: `{"path":"file.go"}`}}}}}},
		{Kind: sessionstore.ItemHostRecord, Data: sessionstore.HostRecord{Version: 1, Kind: sessionstore.HostContext, Context: &sessionstore.ContextRecord{Text: body, Usage: &llm.Usage{InputTokens: 5}, Conversation: &llm.ConversationRecord{Usage: &llm.Usage{OutputTokens: 2}}}}},
		{Kind: sessionstore.ItemToolCallStatus, Data: sessionstore.ToolCallStatus{CallID: "call", Operations: []operation.Operation{{ID: "read", Type: "read", ToolName: "Read", Status: operation.StatusCompleted, State: []byte(`{"old":"opaque"}`)}}}},
	}
	for range 140 {
		items = append(items, sessionstore.Item{Kind: sessionstore.ItemInput, Data: inbox.Input{ID: "tail", Kind: inbox.InputExternal, Payload: []byte(`"recent full body"`)}})
	}
	for i := range items {
		items[i].Sequence = sessionstore.Sequence(i + 1)
	}
	s.items = items
	before, err := json.Marshal(s.items)
	if err != nil {
		t.Fatal(err)
	}
	v, err := s.Observe(t.Context(), 128)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("private old body")) || bytes.Contains(encoded, []byte("opaque")) {
		t.Fatal("observation retained old bodies")
	}
	var public View
	if err := json.Unmarshal(encoded, &public); err != nil {
		t.Fatal("observation must survive the actual gateway codec", err)
	}
	if public.History.NextAfter != sessionstore.Sequence(len(items)) || public.History.More || len(public.History.Items) != len(items) {
		t.Fatal("observation changed canonical cursor")
	}
	for i := len(items) - 128; i < len(items); i++ {
		if !reflect.DeepEqual(public.History.Items[i], ProjectHistoryItem(items[i])) {
			t.Fatal("recent transcript changed", i)
		}
	}
	response := public.History.Items[1].Data.(sessionstore.ModelResponse)
	if response.Response.ID != "response" || response.Response.Model != "model" || response.Response.Usage.InputTokens != 12 || response.Response.Output[1].Data.(llm.ToolCall).Arguments != `{"path":"file.go"}` {
		t.Fatal("activity, usage or target linkage lost")
	}
	if response.Response.Output[0].Data.(llm.Message).Text != strings.Repeat("a", 159)+"👨‍👩‍👧‍👦" {
		t.Fatal("grapheme crossing the display boundary was cut")
	}
	after, _ := json.Marshal(s.items)
	if !bytes.Equal(before, after) {
		t.Fatal("observation changed canonical in-memory values")
	}
	full, err := s.Inspect(0, len(items))
	if err != nil {
		t.Fatal(err)
	}
	if got := full.History.Items[1].Data.(sessionstore.ModelResponse).Response.Output[0].Data.(llm.Message).Text; got != body {
		t.Fatal("explicit history lost original text")
	}
}

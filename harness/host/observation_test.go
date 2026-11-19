package host

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func TestObservePagesKeepsFixedCursorAndLeavesOperationsForSubscribe(t *testing.T) {
	items := make([]sessionstore.Item, 2050)
	for i := range items {
		items[i] = sessionstore.Item{Sequence: sessionstore.Sequence(i + 1), Kind: sessionstore.ItemInput,
			Data: input(fmt.Sprint(i), strings.Repeat("long text ", 100))}
	}
	op := operation.Operation{ID: "work", Status: operation.StatusCompleted, State: []byte("{}")}
	s := &Session{
		ID: "s", Generation: "generation", revision: 7, items: items,
		snapshot:   sessionstore.Snapshot{Session: session.Session{ID: "s"}},
		operations: map[operation.ID]operation.Operation{op.ID: op},
	}
	pages, cursor := 0, sessionstore.BeforeFirst
	err := s.ObservePages(t.Context(), 2, func(v View) error {
		pages++
		if len(v.Operations) != 0 || v.Generation != s.Generation || v.Revision != 7 ||
			v.History.More != (pages < 3) || len(v.History.Items) > 1024 {
			t.Fatal("observation did not keep the original prefix or included Operations", pages)
		}
		if pages == 1 {
			if !s.mu.TryLock() {
				t.Fatal("stream retained the Session lock while sending")
			}
			s.items = append(s.items, sessionstore.Item{Sequence: 2051, Kind: sessionstore.ItemInput, Data: input("during", "during observation")})
			s.revision++
			s.mu.Unlock()
		}
		for _, item := range v.History.Items {
			cursor++
			if item.Sequence != cursor {
				t.Fatal("observation lost a sequence", cursor, item.Sequence)
			}
			body := item.Data.(inbox.Input).Payload
			long := len(body) > 500
			if long != (cursor > 2048) {
				t.Fatal("observation did not preserve only the requested recent bodies", cursor)
			}
		}
		if cursor != v.History.NextAfter {
			t.Fatal("page cursor does not match its items")
		}
		return nil
	})
	if err != nil || pages != 3 || cursor != 2050 {
		t.Fatal("observation did not page the full prefix", err, pages, cursor)
	}
	sub, err := s.Subscribe(cursor, 128, 32)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Cancel()
	v := sub.Initial
	if v.Revision != 8 || len(v.History.Items) != 1 || v.History.Items[0].Sequence != 2051 ||
		len(v.Operations) != 1 || v.Operations[0].ID != op.ID {
		t.Fatal("subscribe lost operations or concurrent append", v)
	}
}

func TestObservationRetainsMetadataRecentHistoryAndCanonicalBodies(t *testing.T) {
	s := &Session{ID: "s", snapshot: sessionstore.Snapshot{Session: session.Session{ID: "s"}}}
	body := strings.Repeat("a", 159) + "👨‍👩‍👧‍👦" + strings.Repeat("private old body ", 2000)
	usage := llm.Usage{InputTokens: 12, OutputTokens: 3, Unknown: []llm.UsageField{llm.UsageCachedInput}, Raw: []byte(`{"attribution":{"items":{"old-raw-usage":12}}}`)}
	timing := &llm.GenerationStatistics{Model: "model", Source: "provider_api", DurationMicros: 500000, Tokens: llm.TokenCounts{Output: 3, OutputKnown: true}}
	items := []sessionstore.Item{
		{Kind: sessionstore.ItemInput, Data: input("old", body)},
		{Kind: sessionstore.ItemModelResponse, Data: sessionstore.ModelResponse{TurnID: "turn", Response: llm.Response{ID: "response", Model: "model", Usage: usage, Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: body}}, {Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "call", Name: "Read", Arguments: `{"path":"file.go"}`}}}}}},
		{Kind: sessionstore.ItemHostRecord, Data: sessionstore.HostRecord{Version: 1, Kind: sessionstore.HostContext, Context: &sessionstore.ContextRecord{Text: body, Usage: &llm.Usage{InputTokens: 5}, Conversation: &llm.ConversationRecord{Usage: &llm.Usage{OutputTokens: 2}}}}},
		{Kind: sessionstore.ItemToolCallStatus, Data: sessionstore.ToolCallStatus{CallID: "call", Operations: []operation.Operation{{ID: "read", Type: "read", ToolName: "Read", Status: operation.StatusCompleted, State: []byte(`{"old":"opaque"}`)}}}},
	}
	for range 140 {
		items = append(items, sessionstore.Item{Kind: sessionstore.ItemInput, Data: inbox.Input{ID: "tail", Kind: inbox.InputExternal, Payload: []byte(`"recent full body"`)}})
	}
	for i := range items {
		items[i].Sequence = sessionstore.Sequence(i + 1)
	}
	data := items[1].Data.(sessionstore.ModelResponse)
	data.Response.Generation = timing
	items[1].Data = data
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
	if bytes.Contains(encoded, []byte("private old body")) || bytes.Contains(encoded, []byte("opaque")) || bytes.Contains(encoded, []byte("old-raw-usage")) {
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
	if string(response.Response.Usage.Raw) != "{}" || !reflect.DeepEqual(response.Response.Usage.Unknown, usage.Unknown) {
		t.Fatal("observation lost reported-usage metadata")
	}
	if response.Response.Generation == nil || *response.Response.Generation != *timing {
		t.Fatal("observation lost output speed metadata")
	}
	projection := v.History.Items[1].Data.(sessionstore.ModelResponse)
	projection.Response.Usage.Unknown[0] = llm.UsageOutput
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
	if got := full.History.Items[1].Data.(sessionstore.ModelResponse).Response.Usage; !reflect.DeepEqual(got, usage) {
		t.Fatal("explicit history lost original usage")
	}
}

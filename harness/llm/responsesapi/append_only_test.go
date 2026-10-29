package responsesapi

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func prepareAppendHistory(h *repairHistory, adapter llm.Adapter, force bool, records *[]sessionstore.ContextRecord) error {
	return h.b.(interface {
		PrepareContext(context.Context, llm.Adapter, llm.RequestOptions, func(sessionstore.ContextRecord) error, bool) error
	}).PrepareContext(h.t.Context(), adapter, llm.RequestOptions{CacheKey: "one-session"}, func(r sessionstore.ContextRecord) error {
		if records != nil {
			*records = append(*records, r)
		}
		h.item(sessionstore.ItemHostRecord, sessionstore.HostRecord{Version: 1, Kind: sessionstore.HostContext, Context: &r}, func() {
			h.b.(interface {
				ApplyContext(sessionstore.ContextRecord)
			}).ApplyContext(r)
		})
		return nil
	}, force)
}

func appendWire(t *testing.T, request llm.Request) map[string]jsontext.Value {
	t.Helper()
	raw, err := requestBody(request, "stable-key", nil)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	return fields
}

func assertWirePrefix(t *testing.T, old, new map[string]jsontext.Value) {
	t.Helper()
	for _, field := range []string{"instructions", "tools"} {
		if !bytes.Equal(old[field], new[field]) {
			t.Fatalf("%s changed", field)
		}
	}
	// JSON array delimiters are framing; compare the actual item bytes without
	// the old closing bracket, which the next appended item replaces with a comma.
	before := bytes.TrimSuffix(old["input"], []byte("]"))
	if !bytes.HasPrefix(new["input"], before) {
		t.Fatal("Responses input is not a byte-for-byte extension")
	}
	var oldItems, newItems []jsontext.Value
	if err := json.Unmarshal(old["input"], &oldItems); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(new["input"], &newItems); err != nil {
		t.Fatal(err)
	}
	if len(oldItems) == 0 || len(newItems) < len(oldItems) || !bytes.Equal(oldItems[0], newItems[0]) {
		t.Fatal("system instructions changed")
	}
}

func TestResponsesAppendOnlyBytePrefixToolsLargeResultChildAndReasoning(t *testing.T) {
	h := newRepairHistory(t, contextengine.Config{})
	for _, name := range []string{"z-tool", "a-tool"} {
		h.b.AddTool(llm.Tool{Type: llm.ToolFunction, Name: name, Parameters: map[string]any{"type": "object", "properties": map[string]any{"z": map[string]any{"type": "string"}, "a": map[string]any{"type": "number"}}}})
	}
	h.user("task", "Inspect files and the child result")
	capture := func(turn string) map[string]jsontext.Value {
		t.Helper()
		if err := prepareAppendHistory(h, nil, false, nil); err != nil {
			t.Fatal(err)
		}
		built, _, err := h.request(turn)
		if err != nil {
			t.Fatal(err)
		}
		return appendWire(t, built.Request)
	}
	first := capture("t1")
	h.response("t1", repairReasoning("reason-1", 128), repairCall("read"))
	full := "HEAD" + strings.Repeat("middle-content ", 6000) + "TAIL"
	h.receipt("t1", "read", full, operation.StatusCompleted)
	second := capture("t2")
	assertWirePrefix(t, first, second)
	if !bytes.Contains(second["input"], []byte("Unit ID")) || !bytes.Contains(second["input"], []byte("HEAD")) || !bytes.Contains(second["input"], []byte("TAIL")) || bytes.Contains(second["input"], []byte(full)) {
		t.Fatal("large result was not fixed to head/tail with full-output reference")
	}
	h.response("t2", llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "Waiting for child."}})
	payload, _ := json.Marshal(inbox.PeerMessage{Version: 1, Sender: "child-1", Target: "parent", Handle: "child-op", Kind: "ended", Text: "Child completed."})
	peer := inbox.Input{ID: "peer", Kind: inbox.InputPeer, Payload: payload}
	h.item(sessionstore.ItemInput, peer, func() {
		if err := h.b.(interface{ AddPeerInput(inbox.Input) error }).AddPeerInput(peer); err != nil {
			t.Fatal(err)
		}
	})
	third := capture("t3")
	assertWirePrefix(t, second, third)
	if !bytes.Contains(third["input"], []byte("encrypted_content")) || !bytes.Contains(third["input"], []byte("reason-1")) {
		t.Fatal("reasoning replay lost its encrypted bytes or ID")
	}
	fourth := capture("t4")
	assertWirePrefix(t, third, fourth)
}

func TestResponsesToolEpochBreaksPrefixOnceThenAppendsAgain(t *testing.T) {
	h := newRepairHistory(t, contextengine.Config{ToolResultKeepTokens: 1000, ToolResultMinPruneTokens: 3000})
	h.user("task", "Keep user and reasoning")
	previous, _, err := h.request("start")
	if err != nil {
		t.Fatal(err)
	}
	prior := appendWire(t, previous.Request)
	turn := "start"
	boundaries := 0
	for i := range 3 {
		id := fmt.Sprintf("call-%d", i)
		call := repairCall(id)
		call.Data = llm.ToolCall{CallID: id, Name: "Bash", Arguments: `{"command":"printf unknown"}`}
		h.response(turn, repairReasoning(fmt.Sprintf("reason-%d", i), 128), call)
		h.receipt(turn, id, strings.Repeat("X", 2200), operation.StatusCompleted)
		ack := fmt.Sprintf("ack-%d", i)
		var records []sessionstore.ContextRecord
		if err := prepareAppendHistory(h, nil, false, &records); err != nil {
			t.Fatal(err)
		}
		for _, r := range records {
			if r.Kind == "prune" {
				boundaries++
				if i != 2 || len(r.Projections) != 2 {
					t.Fatal("wrong epoch", i, r)
				}
			}
		}
		before, wire, err := h.request(ack)
		if err != nil {
			t.Fatal(err)
		}
		assertRepairPairs(t, wire)
		if i < 2 {
			assertWirePrefix(t, prior, appendWire(t, before.Request))
		} else if bytes.HasPrefix(appendWire(t, before.Request)["input"], bytes.TrimSuffix(prior["input"], []byte("]"))) {
			t.Fatal("prune did not replace old result bytes")
		}
		h.response(ack, llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "Received."}})
		records = nil
		if err := prepareAppendHistory(h, nil, false, &records); err != nil {
			t.Fatal(err)
		}
		for _, r := range records {
			if r.Kind == "prune" {
				t.Fatal("second boundary after acknowledgement")
			}
		}
		turn = fmt.Sprintf("next-%d", i)
		after, wire, err := h.request(turn)
		if err != nil {
			t.Fatal(err)
		}
		assertRepairPairs(t, wire)
		next := appendWire(t, after.Request)
		assertWirePrefix(t, appendWire(t, before.Request), next)
		if i == 2 {
			for j := 0; j < 3; j++ {
				if !bytes.Contains(next["input"], []byte(fmt.Sprintf("reason-%d", j))) {
					t.Fatal("reasoning lost")
				}
			}
			if !bytes.Contains(next["input"], []byte("Keep user and reasoning")) {
				t.Fatal("user input lost")
			}
		}
		prior = next
	}
	if boundaries != 1 {
		t.Fatal("expected one epoch", boundaries)
	}
	h.response(turn, llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "Continuing after epoch."}})
	h.user("next", "Keep going")
	after, _, err := h.request("last")
	if err != nil {
		t.Fatal(err)
	}
	assertWirePrefix(t, prior, appendWire(t, after.Request))
}

type appendSummaryAdapter struct {
	calls []llm.Request
	check func(llm.Request)
}

func (a *appendSummaryAdapter) Respond(_ context.Context, r llm.Request, _ llm.RequestOptions) (llm.Response, error) {
	a.calls = append(a.calls, r)
	if a.check != nil {
		a.check(r)
	}
	return llm.Response{Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "Task, constraints and earlier results preserved."}}}}, nil
}

func TestResponsesCompactionPrunesThenSummarizesAndResumesPrefix(t *testing.T) {
	h := newRepairHistory(t, contextengine.Config{})
	h.user("task", "Preserve this task")
	if _, _, err := h.request("t1"); err != nil {
		t.Fatal(err)
	}
	h.response("t1", repairCall("read"))
	h.receipt("t1", "read", strings.Repeat("old tool result ", 1000), operation.StatusCompleted)
	if _, _, err := h.request("ack"); err != nil {
		t.Fatal(err)
	}
	h.response("ack", llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "Received."}})
	for i := range 8 {
		id := fmt.Sprintf("old-%d", i)
		h.user(id, strings.Repeat("old conversation ", 1600))
		h.item(sessionstore.ItemTurn, session.Turn{ID: session.TurnID(id), Type: session.TurnRegular}, h.b.Commit)
		h.response(id, llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "Historical answer."}})
	}
	h.user("latest", "Continue with the task")
	adapter := &appendSummaryAdapter{check: func(r llm.Request) {
		original, err := h.b.Build()
		if err != nil {
			t.Fatal(err)
		}
		assertWirePrefix(t, appendWire(t, original.Request), appendWire(t, r))
		if len(r.Input) != len(original.Request.Input)+1 {
			t.Fatal("summary must append exactly one instruction")
		}
	}}
	var records []sessionstore.ContextRecord
	if err := prepareAppendHistory(h, adapter, false, &records); err != nil {
		t.Fatal(err)
	}
	prune, summary := -1, -1
	for i, r := range records {
		if r.Kind == "prune" {
			prune = i
		}
		if r.Kind == "summary" {
			summary = i
		}
	}
	if prune < 0 || summary <= prune || len(adapter.calls) != 1 {
		t.Fatalf("prune/summary order %d/%d, calls %d", prune, summary, len(adapter.calls))
	}
	summaryRequest := adapter.calls[0]
	// Summary input uses precisely the derived conversation, plus one instruction.
	original := summaryRequest
	original.Input = original.Input[:len(original.Input)-1]
	assertWirePrefix(t, appendWire(t, original), appendWire(t, summaryRequest))
	compacted, _, err := h.request("after-compaction")
	if err != nil {
		t.Fatal(err)
	}
	first := appendWire(t, compacted.Request)
	if bytes.Contains(first["input"], []byte(strings.Repeat("old conversation ", 1600))) && len(compacted.Request.Input) > 20 {
		t.Fatal("view did not advance")
	}
	h.response("after-compaction", llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "Continuing."}})
	h.user("next", "Next step")
	if err := prepareAppendHistory(h, adapter, false, nil); err != nil {
		t.Fatal(err)
	}
	next, _, err := h.request("next-turn")
	if err != nil {
		t.Fatal(err)
	}
	assertWirePrefix(t, first, appendWire(t, next.Request))
	// Old canonical units are still indexed with the full receipt, not its pruned view.
	found := false
	for i := 1; i < 100; i++ {
		if u, ok := h.engine.ResolveUnit(fmt.Sprintf("unit-%020d", i)); ok && strings.Contains(contextengine.Text(u.Item), strings.Repeat("old tool result ", 1000)) {
			found = true
		}
	}
	if !found {
		t.Fatal("compaction discarded original evidence")
	}
}

func TestResponsesModelSwitchThenAppendOnlyBytePrefix(t *testing.T) {
	h := newRepairHistory(t, contextengine.Config{})
	h.user("task", "Inspect files")
	if _, _, err := h.request("t1"); err != nil {
		t.Fatal(err)
	}
	h.response("t1", repairReasoning("old-private", 128), repairCall("read"))
	h.receipt("t1", "read", "Original result", operation.StatusCompleted)
	before, _, err := h.request("t2")
	if err != nil {
		t.Fatal(err)
	}
	h.b.SetModel(llm.Model{ID: "different-model"})
	if err := prepareAppendHistory(h, nil, false, nil); err != nil {
		t.Fatal(err)
	}
	switched, _, err := h.request("switched")
	if err != nil {
		t.Fatal(err)
	}
	wire := appendWire(t, switched.Request)
	if bytes.Contains(wire["input"], []byte("old-private")) || bytes.Equal(appendWire(t, before.Request)["input"], wire["input"]) {
		t.Fatal("model switch did not port prior reasoning")
	}
	h.response("switched", llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "New model response."}})
	h.user("next", "Continue")
	if err := prepareAppendHistory(h, nil, false, nil); err != nil {
		t.Fatal(err)
	}
	next, _, err := h.request("next")
	if err != nil {
		t.Fatal(err)
	}
	assertWirePrefix(t, wire, appendWire(t, next.Request))
}

func TestResponsesPrunerFinishesWithoutSummaryAndStateDoesNotRepeat(t *testing.T) {
	h := newRepairHistory(t, contextengine.Config{})
	h.user("task", "Read the output")
	if _, _, err := h.request("t1"); err != nil {
		t.Fatal(err)
	}
	h.response("t1", repairCall("read"))
	h.receipt("t1", "read", strings.Repeat("large tool result ", 1000), operation.StatusCompleted)
	if _, _, err := h.request("ack"); err != nil {
		t.Fatal(err)
	}
	response := llm.Response{Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "Received the output."}}}, Usage: llm.Usage{InputTokens: 700000}}
	h.item(sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "ack", Response: response}, func() { h.b.AddModelResponse(response) })
	adapter := &appendSummaryAdapter{}
	var records []sessionstore.ContextRecord
	if err := prepareAppendHistory(h, adapter, false, &records); err != nil {
		t.Fatal(err)
	}
	pruned, state := 0, 0
	for _, r := range records {
		if r.Kind == "prune" {
			pruned++
		}
		if r.Kind == "state" {
			state++
		}
	}
	if pruned != 1 || state != 0 || len(adapter.calls) != 0 {
		t.Fatal("pruner did not finish before summary", pruned, state, len(adapter.calls))
	}
	records = nil
	if err := prepareAppendHistory(h, adapter, false, &records); err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatal("unchanged state or projections were appended twice")
	}
}

func TestResponsesPendingReceiptThenTerminalUpdateKeepsBytePrefix(t *testing.T) {
	h := newRepairHistory(t, contextengine.Config{})
	h.user("task", "Inspect file while the child works")
	if _, _, err := h.request("t1"); err != nil {
		t.Fatal(err)
	}
	h.response("t1", repairReasoning("r1", 128), repairCall("async"))
	h.receipt("t1", "async", "running", operation.StatusAwaiting)
	pending, wire, err := h.request("t2")
	if err != nil {
		t.Fatal(err)
	}
	assertRepairPairs(t, wire)
	h.response("t2", llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "Working independently."}})
	h.receipt("t1", "async", `{"content":"FINAL_RESULT","revision":"exact"}`, operation.StatusCompleted)
	terminal, wire, err := h.request("t3")
	if err != nil {
		t.Fatal(err)
	}
	assertWirePrefix(t, appendWire(t, pending.Request), appendWire(t, terminal.Request))
	assertRepairPairs(t, wire)
	count := 0
	for _, item := range wire {
		if item["type"] == "function_call_output" && item["call_id"] == "async" {
			count++
		}
	}
	if count != 1 || !strings.Contains(repairWireText(wire), "FINAL_RESULT") {
		t.Fatal("async update lost result or duplicated native receipt", count)
	}
}

func TestResponsesUnsubmittedPendingStatusKeepsOneNativeReceipt(t *testing.T) {
	h := newRepairHistory(t, contextengine.Config{})
	h.user("task", "Inspect file")
	if _, _, err := h.request("t1"); err != nil {
		t.Fatal(err)
	}
	h.response("t1", repairCall("read"))
	h.receipt("t1", "read", "running", operation.StatusAwaiting)
	h.receipt("t1", "read", `{"content":"FINAL_CONTENT","revision":"exact"}`, operation.StatusCompleted)
	_, wire, err := h.request("t2")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, item := range wire {
		if item["type"] == "function_call_output" && item["call_id"] == "read" {
			count++
		}
	}
	if count != 1 || !strings.Contains(repairWireText(wire), "FINAL_CONTENT") {
		t.Fatal("pending metadata replaced or duplicated final native receipt", count)
	}
}

func TestResponsesSummaryRetainsInterleavedCallResultPair(t *testing.T) {
	h := newRepairHistory(t, contextengine.Config{LargeToolTokens: 100000})
	h.user("old-task", strings.Repeat("a", 180000))
	h.item(sessionstore.ItemTurn, session.Turn{ID: "t1", Type: session.TurnRegular}, h.b.Commit)
	h.response("t1", repairCall("read"))
	h.user("interleaved", strings.Repeat("b", 30000))
	h.receipt("t1", "read", "exact receipt", operation.StatusCompleted)
	h.item(sessionstore.ItemTurn, session.Turn{ID: "ack", Type: session.TurnRegular}, h.b.Commit)
	h.response("ack", llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "Received."}})
	h.user("latest", strings.Repeat("c", 25000))
	adapter := &appendSummaryAdapter{}
	if err := prepareAppendHistory(h, adapter, false, nil); err != nil {
		t.Fatal(err)
	}
	if len(adapter.calls) != 1 {
		t.Fatal("expected summary")
	}
	_, wire, err := h.request("after-summary")
	if err != nil {
		t.Fatal(err)
	}
	assertRepairPairs(t, wire)
	if !strings.Contains(repairWireText(wire), "exact receipt") {
		t.Fatal("recent interleaved receipt missing")
	}
}

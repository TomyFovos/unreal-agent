package responsesapi

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/contextstate"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

func prepareAppendHistory(h *repairHistory, force bool) error {
	return h.b.(interface {
		PrepareContext(context.Context, func(sessionstore.ContextRecord) error, bool) error
	}).PrepareContext(h.t.Context(), func(r sessionstore.ContextRecord) error {
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

func TestResponsesWindowAppendsPublicHistoryWithoutCompaction(t *testing.T) {
	h := newRepairHistory(t, contextengine.Config{})
	h.b.(interface {
		SetWindowSource(session.ID, func(context.Context) (contextstate.State, sessionstore.Sequence, error))
	}).SetWindowSource("fixture", nil)
	for _, name := range []string{"z-tool", "a-tool"} {
		h.b.AddTool(llm.Tool{Type: llm.ToolFunction, Name: name, Parameters: map[string]any{"type": "object", "properties": map[string]any{"z": map[string]any{"type": "string"}, "a": map[string]any{"type": "number"}}}})
	}
	h.user("task", "Inspect files and the child result")
	capture := func(turn string) map[string]jsontext.Value {
		t.Helper()
		if err := prepareAppendHistory(h, false); err != nil {
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
	status := sessionstore.ToolCallStatus{TurnID: "t1", CallID: "read", Status: tool.CallStatus{Error: full}}
	h.item(sessionstore.ItemToolCallStatus, status, func() {
		h.b.AddToolResult("read", []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: full}}, false)
	})
	second := capture("t2")
	assertWirePrefix(t, first, second)
	if !bytes.Contains(second["input"], []byte("rec:fixture/5/tool-error")) || !bytes.Contains(second["input"], []byte("HEAD")) || !bytes.Contains(second["input"], []byte("TAIL")) || bytes.Contains(second["input"], []byte(full)) {
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
	if bytes.Contains(third["input"], []byte("encrypted_content")) || bytes.Contains(third["input"], []byte("reason-1")) {
		t.Fatal("private reasoning entered the public window")
	}
	if _, sent := third["context_management"]; sent {
		t.Fatal("Responses request asked for server-side context compaction")
	}
	fourth := capture("t4")
	assertWirePrefix(t, third, fourth)
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
	if err := prepareAppendHistory(h, false); err != nil {
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
	if err := prepareAppendHistory(h, false); err != nil {
		t.Fatal(err)
	}
	next, _, err := h.request("next")
	if err != nil {
		t.Fatal(err)
	}
	assertWirePrefix(t, wire, appendWire(t, next.Request))
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

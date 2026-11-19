package contextbuilder

import (
	"encoding/json/v2"
	"slices"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func TestContinuationAcknowledgesOnlyLatestReceiptVisibleAtTurn(t *testing.T) {
	f := newContextFixture(t, "openai-codex", contextengine.Config{})
	turn := func(id session.TurnID) {
		f.item(t, sessionstore.ItemTurn, session.Turn{ID: id, Type: session.TurnRegular})
	}
	respond := func(id session.TurnID, response llm.Response) {
		f.item(t, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: id, Response: response})
	}
	turn("start")
	respond("start", llm.Response{Output: []llm.Item{{Type: llm.ItemToolCall,
		Data: llm.ToolCall{CallID: "call", Name: "read", Arguments: "{}"}}}})
	status := sessionstore.ToolCallStatus{TurnID: "start", CallID: "call"}
	f.item(t, sessionstore.ItemToolCallStatus, status)
	turn("old-receipt")
	f.item(t, sessionstore.ItemToolCallStatus, status)
	respond("old-receipt", llm.Response{})
	turn("failed")
	respond("failed", llm.Response{Failure: &llm.Failure{Message: "retry"}})
	groups, err := f.b.requiredContinuations()
	if err != nil || len(groups) != 1 {
		t.Fatalf("latest receipt was not consumed: groups=%v err=%v", groups, err)
	}
	turn("read-receipt")
	respond("read-receipt", llm.Response{})
	groups, err = f.b.requiredContinuations()
	if err != nil || len(groups) != 0 {
		t.Fatalf("consumed receipt remained required: groups=%v err=%v", groups, err)
	}
}

func TestRedactedReceiptUsesOnlyCanonicalState(t *testing.T) {
	for _, tc := range []struct {
		name     string
		present  bool
		statuses []operation.Status
		running  bool
		state    string
	}{
		{"absent", false, nil, false, "unavailable"}, {"completed", true, []operation.Status{operation.StatusCompleted}, false, "completed"},
		{"failed", true, []operation.Status{operation.StatusCompleted, operation.StatusFailed}, false, "failed"},
		{"canceled", true, []operation.Status{operation.StatusCanceled}, false, "canceled"},
		{"running", true, []operation.Status{operation.StatusAwaiting}, true, "running"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := NewBuilder().(*builder)
			if tc.present {
				status := sessionstore.ToolCallStatus{CallID: "PRIVATE_ID"}
				for _, s := range tc.statuses {
					status.Operations = append(status.Operations, operation.Operation{ID: "PRIVATE_ID", Status: s})
				}
				b.source.Data = status
			}
			out, present := b.redactedReceipt(tc.running)
			if present != tc.present || len(out) != 1 || strings.Contains(out[0].Value, "PRIVATE_ID") {
				t.Fatal("redaction leaked or invented canonical state")
			}
			var stub struct {
				ReceiptPresent  bool     `json:"receipt_present"`
				State           string   `json:"state"`
				OperationStates []string `json:"operation_states"`
			}
			if json.Unmarshal([]byte(out[0].Value), &stub) != nil || stub.State != tc.state || stub.ReceiptPresent != tc.present || (stub.State == "unavailable" && slices.Contains(stub.OperationStates, "completed")) {
				t.Fatal("redaction state mismatch")
			}
		})
	}
}

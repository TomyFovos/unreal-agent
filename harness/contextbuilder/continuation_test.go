package contextbuilder

import (
	"encoding/json/v2"
	"slices"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

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

package contextbuilder

import (
	"encoding/json/v2"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	files "github.com/unreallabsai/unreal-agent/harness/native"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	nativetool "github.com/unreallabsai/unreal-agent/harness/tool/native"
)

type receiptStateRecorder struct{ spec operation.Spec }

func (r *receiptStateRecorder) Submit(spec operation.Spec) operation.ID {
	r.spec = spec
	return "private-operation-id"
}

type receiptStateStub struct {
	Redacted        bool     `json:"redacted"`
	ReceiptPresent  bool     `json:"receipt_present"`
	State           string   `json:"state"`
	OperationStates []string `json:"operation_states"`
}

// Use the production translator's WaitingFor, including after reconciliation.
// The remote handle is synthetic, but has the actual typed native read shape.
func translatedSecretReceipt(t *testing.T, s operation.Status) (sessionstore.ToolCallStatus, llm.ToolResult) {
	t.Helper()
	translator := nativetool.New("read")
	r := &receiptStateRecorder{}
	call := llm.ToolCall{CallID: "secret-read", Name: "read", Arguments: `{"path":"fixture.txt"}`}
	status := translator.Translate(r, call)
	if status.Error != "" || len(status.WaitingFor) != 1 {
		t.Fatal("native translation did not submit one dependency")
	}
	op := operation.Operation{ID: status.WaitingFor[0], Type: r.spec.Type, Version: r.spec.Version, MaxOutputLength: r.spec.MaxOutputLength, State: r.spec.State, Status: s}
	state, err := operation.DecodeRemoteJobState(op)
	if err != nil {
		t.Fatal(err)
	}
	state.Handle = files.EncodeResult(files.Result{Version: files.Version, Code: "ok", Content: "Authorization: Bearer SYNTHETIC_SECRET_BODY"})
	step, err := operation.UpdateRemoteJob(op, state, s)
	if err != nil {
		t.Fatal(err)
	}
	ops := []operation.Operation{*step.Operation}
	result, err := translator.TranslateResult(call.CallID, status, ops)
	if err != nil {
		t.Fatal(err)
	}
	return sessionstore.ToolCallStatus{TurnID: "secret-turn", CallID: call.CallID, Status: status, Operations: ops}, result
}

func decodeReceiptStub(t *testing.T, text string) receiptStateStub {
	t.Helper()
	var stub receiptStateStub
	if json.Unmarshal([]byte(text), &stub, json.RejectUnknownMembers(true)) != nil || !stub.Redacted {
		t.Fatal("expected a typed redaction stub")
	}
	return stub
}

func TestReceiptStateRepairNativeTerminalWaitingFor(t *testing.T) {
	for _, s := range []operation.Status{operation.StatusCompleted, operation.StatusFailed, operation.StatusCanceled} {
		t.Run(string(s), func(t *testing.T) {
			status, result := translatedSecretReceipt(t, s)
			if _, safe := contextengine.PublicItem(llm.Item{Type: llm.ItemToolResult, Data: result}); safe {
				t.Fatal("fixture did not exercise the actual secretguard")
			}
			before, _ := json.Marshal(status)
			makeFixture := func() *contextFixture {
				f := newContextFixture(t, "openai-codex", contextengine.Config{})
				f.user(t, "input", "Inspect fixture.txt")
				f.item(t, sessionstore.ItemTurn, session.Turn{ID: status.TurnID, Type: session.TurnRegular})
				f.item(t, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: status.TurnID, Response: llm.Response{Output: []llm.Item{{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: status.CallID, Name: "read", Arguments: `{"path":"fixture.txt"}`}}}}})
				return f
			}
			apply := func(f *contextFixture) {
				f.sequence++
				item := sessionstore.Item{Kind: sessionstore.ItemToolCallStatus, Sequence: f.sequence, Data: status}
				f.b.SetHistoryItem(item)
				f.b.AddToolResult(status.CallID, result.Output, false)
				f.b.ObserveHistory(item)
			}
			var first receiptStateStub
			for _, mode := range []string{"live", "replay", "resume"} {
				f := makeFixture()
				apply(f)
				if mode == "resume" {
					f.user(t, "resume", "Continue after the failed generation")
				}
				built, err := f.b.Build()
				if err != nil {
					t.Fatal(err)
				}
				var found bool
				for _, item := range built.Request.Input {
					if r, ok := item.Data.(llm.ToolResult); ok && r.CallID == status.CallID {
						stub := decodeReceiptStub(t, r.Output[0].Value)
						if !stub.ReceiptPresent || stub.State != string(s) || !slices.Equal(stub.OperationStates, []string{string(s)}) {
							t.Fatalf("%s: state=%s operation_states=%v, expected %s", mode, stub.State, stub.OperationStates, s)
						}
						if mode == "live" {
							first = stub
						} else if !reflect.DeepEqual(first, stub) {
							t.Fatal("canonical replay changed the state projection")
						}
						found = true
					}
				}
				if !found || strings.Contains(requestText(built), "SYNTHETIC_SECRET_BODY") {
					t.Fatal("safe receipt absent or private body leaked")
				}
			}
			after, _ := json.Marshal(status)
			if string(before) != string(after) || len(status.Status.WaitingFor) != 1 {
				t.Fatal("canonical WaitingFor or Operation changed")
			}
		})
	}
}

func TestReceiptStateRepairAggregateIsOrderIndependent(t *testing.T) {
	for _, tc := range []struct {
		name     string
		states   []operation.Status
		error    string
		denied   bool
		running  bool
		present  bool
		expected string
	}{
		{"missing", nil, "", false, false, false, "unavailable"},
		{"active", []operation.Status{operation.StatusCompleted, operation.StatusAwaiting}, "", false, true, true, "running"},
		{"rejected", nil, "private-error", true, false, true, "rejected"},
		{"translation-failed", nil, "private-error", false, false, true, "failed"},
		{"failed-canceled", []operation.Status{operation.StatusFailed, operation.StatusCanceled, operation.StatusCompleted}, "", false, false, true, "failed"},
		{"canceled-completed", []operation.Status{operation.StatusCompleted, operation.StatusCanceled}, "", false, false, true, "canceled"},
		{"unknown", []operation.Status{operation.StatusCompleted, operation.Status("private-invalid-state")}, "", false, false, true, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := NewBuilder().(*builder)
			status := sessionstore.ToolCallStatus{Status: tool.CallStatus{Error: tc.error, WaitingFor: []operation.ID{"private-dependency"}}}
			if tc.denied {
				status.Status.Denial = &permission.Error{Code: permission.Denied, Capability: "write", Reason: "private-reason"}
			}
			for _, s := range tc.states {
				status.Operations = append(status.Operations, operation.Operation{ID: "private-id", Status: s})
			}
			var first receiptStateStub
			for i := range 2 {
				if tc.present {
					b.source.Data = status
				}
				out, _ := b.redactedReceipt(tc.running)
				stub := decodeReceiptStub(t, out[0].Value)
				if stub.State != tc.expected || stub.ReceiptPresent != tc.present || strings.Contains(out[0].Value, "private-") {
					t.Fatalf("state=%s expected=%s", stub.State, tc.expected)
				}
				if i == 0 {
					first = stub
				} else if !reflect.DeepEqual(first, stub) {
					t.Fatal("Operation enumeration order changed the stub")
				}
				slices.Reverse(status.Operations)
			}
		})
	}
}

//go:build linux || darwin

package claudecode

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/mutation"
	files "github.com/unreallabsai/unreal-agent/harness/native"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

type structuredProjectionHistory struct {
	t       *testing.T
	b       contextbuilder.Builder
	seq     sessionstore.Sequence
	history []sessionstore.Item
}

func newStructuredProjectionHistory(t *testing.T, budget int64) *structuredProjectionHistory {
	t.Helper()
	b := contextbuilder.NewBuilder()
	if _, err := contextbuilder.EnableCompaction(b, contextengine.Config{InputBudget: budget, FallbackWindow: max(32768, budget+8192)}, contextengine.Constraints{}); err != nil {
		t.Fatal(err)
	}
	b.(interface{ SetRuntimeProvider(string) }).SetRuntimeProvider("claude-code")
	b.SetModel(llm.Model{ID: "claude-configured-a", ReasoningEffort: "medium"})
	registry := tool.NewRegistry(tool.StaticTranslators{}, "read", "edit")
	for _, d := range registry.StaticDefinitions() {
		b.AddTool(d.Tool)
	}
	h := &structuredProjectionHistory{t: t, b: b}
	in := inbox.Input{ID: "fixture-task", Kind: inbox.InputExternal, Payload: []byte(`"Inspect the current files before deciding the next Action."`)}
	h.record(sessionstore.ItemInput, in, func() {
		if b.AddExternalInput(in) != nil {
			t.Fatal("user input rejected")
		}
	})
	return h
}

func (h *structuredProjectionHistory) record(kind sessionstore.ItemKind, data any, apply func()) {
	h.seq++
	item := sessionstore.Item{Kind: kind, Sequence: h.seq, Data: data}
	h.b.(interface{ SetHistoryItem(sessionstore.Item) }).SetHistoryItem(item)
	apply()
	h.b.(interface{ ObserveHistory(sessionstore.Item) }).ObserveHistory(item)
	h.history = append(h.history, item)
}

func (h *structuredProjectionHistory) receipt(response llm.Response, text string, state operation.Status) llm.ToolOutcome {
	call := response.Output[0].Data.(llm.ToolCall)
	turn := session.Turn{ID: session.TurnID(fmt.Sprint("turn-", h.seq)), Type: session.TurnRegular}
	h.record(sessionstore.ItemTurn, turn, h.b.Commit)
	h.record(sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: turn.ID, Response: response}, func() { h.b.AddModelResponse(response) })
	status := sessionstore.ToolCallStatus{TurnID: turn.ID, CallID: call.CallID, Status: tool.CallStatus{WaitingFor: []operation.ID{"fixture-op"}}, Operations: []operation.Operation{{ID: "fixture-op", Status: state}}}
	result := llm.ToolResult{CallID: call.CallID, Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: text}}}
	h.record(sessionstore.ItemToolCallStatus, status, func() { h.b.AddToolResult(call.CallID, result.Output, false) })
	return llm.ToolOutcome{Result: result, Failed: state != operation.StatusCompleted}
}

func TestStructuredReceiptProjectionRepairBudgetAndMiddle(t *testing.T) {
	for _, size := range []int{128, 4096, 8192, 12288} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			h := newStructuredProjectionHistory(t, 24576)
			body := strings.Repeat("x", size/2) + "MIDDLE_EDIT_TARGET" + strings.Repeat("y", size/2)
			read := files.Result{Version: files.Version, Code: "ok", Path: "fixture.txt", Content: body, StartLine: 1, Revision: new(mutation.RevisionOf([]byte(body)))}
			encoded := string(files.EncodeResult(read))
			built, err := h.b.Build()
			if err != nil {
				t.Fatal(err)
			}
			schema, err := newActionSchema(built.Request.Tools)
			if err != nil {
				t.Fatal(err)
			}
			opt := llm.RequestOptions{ActionScope: "fixture-scope", InputBudget: 24576}
			opt.RefreshContext = func(_ context.Context, reserve int64) (llm.Request, int64, error) {
				b, err := h.b.(interface {
					BuildWithReserve(int64) (contextbuilder.Result, error)
				}).BuildWithReserve(reserve)
				return b.Request, 24576, err
			}
			opt.Tools = func(_ context.Context, response llm.Response) ([]llm.ToolOutcome, error) {
				return []llm.ToolOutcome{h.receipt(response, encoded, operation.StatusCompleted)}, nil
			}
			generations := 0
			_, err = runStructuredRounds(t.Context(), built.Request, opt, schema, (ToolBridgeConfig{}).limits(), func(_ context.Context, _ llm.Model, s *actionSchema, system, input string) (actionProposal, llm.Response, error) {
				generations++
				if contextengine.Estimate(system)+contextengine.Estimate(input) > opt.InputBudget {
					t.Fatal("final request exceeds the real total input ceiling")
				}
				if generations == 1 {
					p, e := s.parse(jsontext.Value(`{"type":"action","action":{"id":"read-1","tool":"read","arguments":{"path":"fixture.txt"}}}`))
					return p, llm.Response{}, e
				}
				if !strings.Contains(input, "MIDDLE_EDIT_TARGET") || strings.Count(input, "MIDDLE_EDIT_TARGET") != 1 {
					t.Fatal("budget-fit read lost its middle or was injected twice")
				}
				_, observed := structuredProjectionResult(t, input)
				var got files.Result
				if len(observed.Output) != 1 || json.Unmarshal([]byte(observed.Output[0].Value), &got) != nil || got.Content != read.Content || got.Revision == nil || *got.Revision != *read.Revision || got.StartLine != read.StartLine || got.Truncated {
					t.Fatal("final Claude input lost the typed body/revision/range")
				}
				p, e := s.parse(jsontext.Value(layerFinal))
				return p, llm.Response{}, e
			})
			if err != nil || generations != 2 {
				t.Fatal("fit continuation failed", err)
			}
		})
	}
}

func structuredProjectionResult(t *testing.T, input string) (bool, llm.ToolResult) {
	t.Helper()
	const label = "\n\nLast Unreal Action Result (machine-owned canonical receipt; Failed=true means failure):\n"
	parts := strings.SplitN(input, label, 2)
	if len(parts) != 2 {
		t.Fatal("machine-owned latest receipt envelope absent")
	}
	var value llm.ToolOutcome
	if json.Unmarshal([]byte(parts[1]), &value) != nil {
		t.Fatal("latest result envelope is malformed")
	}
	return value.Failed, value.Result
}

func TestStructuredReceiptProjectionRepairSecretState(t *testing.T) {
	for _, state := range []operation.Status{operation.StatusCompleted, operation.StatusFailed, operation.StatusCanceled} {
		t.Run(string(state), func(t *testing.T) {
			h := newStructuredProjectionHistory(t, 24576)
			outcome := h.receipt(llm.Response{Output: []llm.Item{{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "read-secret", Name: "read", Arguments: `{"path":"fixture.txt"}`}}}}, `Authorization: Bearer SYNTHETIC_PRIVATE_MARKER`, state)
			before, _ := json.Marshal(h.history)
			built, err := h.b.Build()
			if err != nil {
				t.Fatal(err)
			}
			_, input, err := structuredPrompt(built.Request.Input, &outcome, 65536)
			if err != nil || strings.Contains(input, "SYNTHETIC_PRIVATE_MARKER") {
				t.Fatal("protected body leaked", err)
			}
			failed, result := structuredProjectionResult(t, input)
			var stub struct {
				Redacted        bool     `json:"redacted"`
				ReceiptPresent  bool     `json:"receipt_present"`
				State           string   `json:"state"`
				OperationStates []string `json:"operation_states"`
			}
			if len(result.Output) != 1 || json.Unmarshal([]byte(result.Output[0].Value), &stub) != nil || !stub.Redacted || !stub.ReceiptPresent || stub.State != string(state) || len(stub.OperationStates) != 1 || stub.OperationStates[0] != string(state) || failed != outcome.Failed {
				t.Fatal("Claude adapter replaced the Builder's actual-state stub")
			}
			after, _ := json.Marshal(h.history)
			if string(before) != string(after) {
				t.Fatal("request projection changed canonical history")
			}
		})
	}
}

func TestStructuredReceiptProjectionRepairMultipleReadsAndReserve(t *testing.T) {
	h := newStructuredProjectionHistory(t, 24576)
	turn := session.Turn{ID: "multi-read", Type: session.TurnRegular}
	h.record(sessionstore.ItemTurn, turn, h.b.Commit)
	var calls []llm.Item
	for _, id := range []string{"read-a", "read-b"} {
		calls = append(calls, llm.Item{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: id, Name: "read", Arguments: `{"path":"` + id + `.txt"}`}})
	}
	h.record(sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: turn.ID, Response: llm.Response{Output: calls}}, func() { h.b.AddModelResponse(llm.Response{Output: calls}) })
	var last llm.ToolOutcome
	for _, id := range []string{"read-a", "read-b"} {
		body := strings.Repeat("x", 2048) + "MIDDLE_" + id + strings.Repeat("y", 2048)
		result := llm.ToolResult{CallID: id, Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: string(files.EncodeResult(files.Result{Version: files.Version, Code: "ok", Path: id + ".txt", Content: body, StartLine: 3, Truncated: true, Revision: new(mutation.RevisionOf([]byte(body)))}))}}}
		status := sessionstore.ToolCallStatus{TurnID: turn.ID, CallID: id, Status: tool.CallStatus{WaitingFor: []operation.ID{operation.ID(id)}}, Operations: []operation.Operation{{ID: operation.ID(id), Status: operation.StatusCompleted}}}
		h.record(sessionstore.ItemToolCallStatus, status, func() { h.b.AddToolResult(id, result.Output, false) })
		last = llm.ToolOutcome{Result: result}
	}
	before, _ := json.Marshal(h.history)
	reserve, err := structuredTransportReserve(&last)
	if err != nil {
		t.Fatal(err)
	}
	copy := last
	copy.Result.Output = []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: strings.Repeat("huge-raw-body", 10000)}}
	other, err := structuredTransportReserve(&copy)
	if err != nil || other != reserve {
		t.Fatal("raw receipt was double charged to the private transport reserve")
	}
	built, err := h.b.(interface {
		BuildWithReserve(int64) (contextbuilder.Result, error)
	}).BuildWithReserve(reserve)
	if err != nil {
		t.Fatal(err)
	}
	system, input, err := structuredPrompt(built.Request.Input, &last, 65536)
	if err != nil || contextengine.Estimate(system)+contextengine.Estimate(input) > 24576 || built.Report.Context.TransportReserve != reserve {
		t.Fatal("private reserve/final transport budget mismatch", err)
	}
	for _, id := range []string{"read-a", "read-b"} {
		if strings.Count(input, "MIDDLE_"+id) != 1 {
			t.Fatal("a current read was dropped or sent twice")
		}
	}
	_, result := structuredProjectionResult(t, input)
	var read files.Result
	if len(result.Output) != 1 || json.Unmarshal([]byte(result.Output[0].Value), &read) != nil || !read.Truncated || read.StartLine != 3 || read.Revision == nil {
		t.Fatal("existing range/truncation provenance changed")
	}
	after, _ := json.Marshal(h.history)
	if string(before) != string(after) {
		t.Fatal("bounded projection rewrote canonical receipts")
	}
}

func TestStructuredReceiptProjectionRepairPortableNeverUsesRaw(t *testing.T) {
	h := newStructuredProjectionHistory(t, 24576)
	h.b.(interface{ SetHistoryProvider(string) }).SetHistoryProvider("openai-codex")
	last := h.receipt(llm.Response{Output: []llm.Item{{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "portable-read", Name: "read", Arguments: `{"path":"fixture.txt"}`}}}}, `Authorization: Bearer SYNTHETIC_PRIVATE_MARKER`, operation.StatusCompleted)
	built, err := h.b.Build()
	if err != nil {
		t.Fatal(err)
	}
	_, input, err := structuredPrompt(built.Request.Input, &last, 65536)
	if err != nil || strings.Contains(input, "SYNTHETIC_PRIVATE_MARKER") {
		t.Fatal("portable projection fell back to raw canonical body", err)
	}
	parts := strings.SplitN(input, structuredResultLabel, 2)
	var items []llm.Item
	if json.Unmarshal([]byte(parts[0]), &items) != nil {
		t.Fatal("public context JSON invalid")
	}
	var found bool
	for _, item := range items {
		message, ok := item.Data.(llm.Message)
		if !ok || !strings.HasPrefix(message.Text, "Prior Unreal tool_result (historical data):\n") {
			continue
		}
		var projected llm.ToolResult
		if json.Unmarshal([]byte(strings.SplitN(message.Text, "\n", 2)[1]), &projected) != nil || len(projected.Output) != 1 {
			t.Fatal("portable receipt malformed")
		}
		var stub struct {
			ReceiptPresent bool   `json:"receipt_present"`
			State          string `json:"state"`
		}
		if json.Unmarshal([]byte(projected.Output[0].Value), &stub) != nil || !stub.ReceiptPresent || stub.State != "completed" {
			t.Fatal("portable redaction lost its real canonical state")
		}
		found = true
	}
	if !found {
		t.Fatal("Builder's portable state stub was discarded")
	}
	_, envelope := structuredProjectionResult(t, input)
	if len(envelope.Output) != 0 {
		t.Fatal("portable body was injected twice into a private envelope")
	}
}

func TestStructuredReceiptProjectionRepairTrueOverflowAndOpaqueCap(t *testing.T) {
	// Mandatory context really exceeds the runtime ceiling: no generation or
	// tool callback may occur. Canonical history retains the original receipt.
	h := newStructuredProjectionHistory(t, 6000)
	h.receipt(llm.Response{Output: []llm.Item{{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "large", Name: "read", Arguments: `{"path":"fixture.txt"}`}}}}, strings.Repeat("opaque-body", 3000), operation.StatusCompleted)
	before, _ := json.Marshal(h.history)
	registry := tool.NewRegistry(tool.StaticTranslators{}, "read", "edit")
	var tools []llm.Tool
	for _, d := range registry.StaticDefinitions() {
		tools = append(tools, d.Tool)
	}
	schema, err := newActionSchema(tools)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	opt := llm.RequestOptions{InputBudget: 6000, Tools: func(context.Context, llm.Response) ([]llm.ToolOutcome, error) { calls++; return nil, nil }, RefreshContext: func(_ context.Context, reserve int64) (llm.Request, int64, error) {
		built, err := h.b.(interface {
			BuildWithReserve(int64) (contextbuilder.Result, error)
		}).BuildWithReserve(reserve)
		return built.Request, 6000, err
	}}
	response, err := runStructuredRounds(t.Context(), llm.Request{Model: llm.Model{ID: "claude-configured-a", ReasoningEffort: "medium"}, Tools: tools}, opt, schema, (ToolBridgeConfig{}).limits(), func(context.Context, llm.Model, *actionSchema, string, string) (actionProposal, llm.Response, error) {
		calls++
		return actionProposal{}, llm.Response{}, nil
	})
	requireCode(t, err, "bridge_context_budget")
	after, _ := json.Marshal(h.history)
	if calls != 0 || len(response.Output) != 0 || string(before) != string(after) {
		t.Fatal("true overflow executed, fabricated Final or mutated receipts")
	}
	for _, body := range []string{strings.Repeat("opaque-body", 1000), `{"content":"` + strings.Repeat("x", 2048) + `","unknown":true}`, `{"content":"` + strings.Repeat("x", 2048) + `","content":"duplicate"}`} {
		out := llm.ToolResult{CallID: "cap", Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: body}}}
		_, err := boundedStructuredReceipt(out, "read", 1024)
		requireCode(t, err, "bridge_context_budget")
		if strings.Contains(err.Error(), "opaque-body") || strings.Contains(err.Error(), "duplicate") {
			t.Fatal("oversized opaque receipt leaked into diagnostics")
		}
	}
	// The explicit cap also applies after a whole-group portable projection.
	portable := contextengine.Portable(contextengine.Unit{Kind: contextengine.ToolResult, Item: llm.Item{Type: llm.ItemToolResult, Data: llm.ToolResult{CallID: "cap", Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: strings.Repeat("opaque", 1000)}}}}}, false)
	_, _, err = structuredPrompt([]llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "task"}}, portable}, nil, 1024)
	requireCode(t, err, "bridge_context_budget")
}

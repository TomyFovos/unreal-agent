package responsesapi

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

// Actual canonical replay -> Builder -> Engine -> JSON-v2 wire. Synthetic only.
type repairHistory struct {
	t   *testing.T
	b   contextbuilder.Builder
	seq sessionstore.Sequence
}

func TestContinuationRepairInvalidProjectionSendsNoHTTP(t *testing.T) {
	var received atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { received.Add(1) }))
	defer server.Close()
	a := newTestAdapter(t, server.URL)
	_, err := a.Respond(t.Context(), llm.Request{Input: []llm.Item{repairCall("missing")}}, llm.RequestOptions{})
	if err == nil || received.Load() != 0 || strings.Contains(err.Error(), "source.go") {
		t.Fatal("invalid continuation escaped pre-send validation")
	}
}

func FuzzContinuationRepairWirePairs(f *testing.F) {
	for _, seed := range []uint8{0, 1, 7, 32, 127, 255} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, seed uint8) {
		h := newRepairHistory(t, contextengine.Config{})
		h.user("task", "Inspect synthetic files")
		if _, _, err := h.request("t1"); err != nil {
			t.Fatal(err)
		}
		var calls []llm.Item
		for i := range int(seed%5) + 1 {
			calls = append(calls, repairCall(fmt.Sprint(i)))
		}
		h.response("t1", calls...)
		for i := range calls {
			h.receipt("t1", fmt.Sprint(i), strings.Repeat("safe fixture ", int(seed)*4+1), operation.StatusCompleted)
		}
		_, wire, err := h.request("t2")
		if err != nil {
			if contextengine.IsBudgetError(err) {
				return
			}
			t.Fatal(err)
		}
		assertRepairPairs(t, wire)
	})
}

func newRepairHistory(t *testing.T, config contextengine.Config) *repairHistory {
	t.Helper()
	b := contextbuilder.NewBuilder()
	if _, err := contextbuilder.EnableCompaction(b, config, contextengine.Constraints{}); err != nil {
		t.Fatal(err)
	}
	b.(interface{ SetRuntimeProvider(string) }).SetRuntimeProvider("openai-codex")
	b.SetModel(llm.Model{ID: "synthetic-model"})
	b.(interface {
		SetContextRuntime(sessionstore.RuntimeSelection, bool)
	}).SetContextRuntime(sessionstore.RuntimeSelection{Provider: "openai-codex", Model: "synthetic-model", ContextWindow: 272000}, true)
	return &repairHistory{t: t, b: b}
}
func (h *repairHistory) item(kind sessionstore.ItemKind, data any, apply func()) {
	h.seq++
	item := sessionstore.Item{Kind: kind, Data: data, Sequence: h.seq, RecordedAt: time.Unix(int64(h.seq), 0)}
	h.b.(interface{ SetHistoryItem(sessionstore.Item) }).SetHistoryItem(item)
	apply()
	h.b.(interface{ ObserveHistory(sessionstore.Item) }).ObserveHistory(item)
}
func (h *repairHistory) user(id, text string) {
	encoded, _ := json.Marshal(text)
	in := inbox.Input{ID: inbox.ID(id), Kind: inbox.InputExternal, Payload: encoded}
	h.item(sessionstore.ItemInput, in, func() {
		if err := h.b.AddExternalInput(in); err != nil {
			h.t.Fatal(err)
		}
	})
}
func (h *repairHistory) request(turn string) (contextbuilder.Result, []map[string]any, error) {
	built, err := h.b.Build()
	if err != nil {
		return built, nil, err
	}
	body, err := requestBody(built.Request, "", nil)
	if err != nil {
		return built, nil, err
	}
	var wire struct {
		Input []map[string]any `json:"input"`
	}
	if err = json.Unmarshal(body, &wire); err != nil {
		h.t.Fatal(err)
	}
	h.item(sessionstore.ItemTurn, session.Turn{ID: session.TurnID(turn), Type: session.TurnRegular}, h.b.Commit)
	return built, wire.Input, nil
}
func (h *repairHistory) response(turn string, items ...llm.Item) {
	r := llm.Response{Stop: llm.StopComplete, Output: items}
	h.item(sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: session.TurnID(turn), Response: r}, func() { h.b.AddModelResponse(r) })
}
func repairCall(id string) llm.Item {
	return llm.Item{Type: llm.ItemToolCall, ProviderID: "fc-" + id, Data: llm.ToolCall{CallID: id, Name: "read", Arguments: `{"path":"source.go"}`}}
}
func repairReasoning(id string, size int) llm.Item {
	raw, _ := json.Marshal(map[string]any{"type": "reasoning", "id": id, "summary": []any{}, "encrypted_content": strings.Repeat("Q", size)})
	return llm.Item{Type: llm.ItemReasoning, ProviderID: id, Data: llm.Reasoning{Raw: raw}}
}
func (h *repairHistory) receipt(turn, id, text string, state operation.Status) {
	running := state == operation.StatusAwaiting
	status := sessionstore.ToolCallStatus{TurnID: session.TurnID(turn), CallID: id, Operations: []operation.Operation{{ID: operation.ID("op-" + id), Status: state}}}
	if running {
		status.Status.WaitingFor = []operation.ID{operation.ID("op-" + id)}
	}
	h.item(sessionstore.ItemToolCallStatus, status, func() {
		h.b.AddToolResult(id, []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: text}}, running)
	})
}
func assertRepairPairs(t *testing.T, items []map[string]any) {
	t.Helper()
	calls, results := map[string]bool{}, map[string]bool{}
	for _, item := range items {
		id, _ := item["call_id"].(string)
		switch item["type"] {
		case "function_call":
			calls[id] = true
		case "function_call_output":
			if !calls[id] {
				t.Fatal("native output lacks preceding call")
			}
			results[id] = true
		}
	}
	for id := range calls {
		if !results[id] {
			t.Fatal("native call lacks following output")
		}
	}
}
func repairWireText(items []map[string]any) string { b, _ := json.Marshal(items); return string(b) }

func TestContinuationRepairReadBodyAndRevision(t *testing.T) {
	for _, size := range []int{128, 4096, 8192} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			h := newRepairHistory(t, contextengine.Config{})
			h.user("request", "inspect source.go")
			if _, _, err := h.request("t1"); err != nil {
				t.Fatal(err)
			}
			h.response("t1", repairCall("read-a"), repairCall("read-b"))
			for _, id := range []string{"read-a", "read-b"} {
				payload, _ := json.Marshal(map[string]any{"content": strings.Repeat("x", size), "revision": "synthetic-exact-revision-" + id, "truncated": false})
				h.receipt("t1", id, string(payload), operation.StatusCompleted)
			}
			built, wire, err := h.request("t2")
			if err != nil {
				t.Fatal(err)
			}
			assertRepairPairs(t, wire)
			for _, id := range []string{"read-a", "read-b"} {
				if !strings.Contains(repairWireText(wire), "synthetic-exact-revision-"+id) {
					t.Fatal("receipt body/revision replaced by reference")
				}
			}
			if built.Report.Context.EstimatedInputTokens > built.Report.Context.Budget.Input {
				t.Fatal("estimate exceeds budget")
			}
		})
	}
}

func TestContinuationRepairSecretReceiptUsesActualStateStub(t *testing.T) {
	for _, state := range []operation.Status{operation.StatusCompleted, operation.StatusFailed, operation.StatusCanceled, operation.StatusAwaiting} {
		t.Run(string(state), func(t *testing.T) {
			h := newRepairHistory(t, contextengine.Config{})
			h.user("request", "inspect source.go")
			if _, _, err := h.request("t1"); err != nil {
				t.Fatal(err)
			}
			h.response("t1", repairCall("read"))
			h.receipt("t1", "read", `Authorization: Bearer SYNTHETIC_PRIVATE_MARKER`, state)
			_, wire, err := h.request("t2")
			if err != nil {
				t.Fatal(err)
			}
			assertRepairPairs(t, wire)
			text := repairWireText(wire)
			want := string(state)
			if state == operation.StatusAwaiting {
				// The public Builder deliberately replaces any running body with
				// its fixed waiting placeholder before compaction sees the payload.
				if strings.Contains(text, "SYNTHETIC_PRIVATE_MARKER") || !strings.Contains(text, "still running") {
					t.Fatal("running placeholder lost its real state")
				}
				return
			}
			if strings.Contains(text, "SYNTHETIC_PRIVATE_MARKER") || !strings.Contains(text, "redacted") || !strings.Contains(text, want) {
				t.Fatal("secret receipt not replaced by state-preserving stub")
			}
		})
	}
}

func TestContinuationRepairAReferenceAndCRetrieval(t *testing.T) {
	t.Run("A-private-replay-pressure", func(t *testing.T) {
		h := newRepairHistory(t, contextengine.Config{})
		h.user("request", "Run the tests")
		if _, _, err := h.request("t1"); err != nil {
			t.Fatal(err)
		}
		h.response("t1", repairReasoning("synthetic-private-replay", 24<<10), repairCall("test"))
		result := strings.Repeat("ok synthetic/package 0.012s\n", 260)
		h.receipt("t1", "test", result, operation.StatusCompleted)
		built, wire, err := h.request("t2")
		if err != nil {
			t.Fatal(err)
		}
		assertRepairPairs(t, wire)
		if built.Report.Context.EstimatedInputTokens > built.Report.Context.Budget.Input || !strings.Contains(repairWireText(wire), "ok synthetic/package") {
			t.Fatal("safe full portable receipt lost or budget exceeded")
		}
	})
	t.Run("C-retrieved-call-recent-receipt", func(t *testing.T) {
		h := newRepairHistory(t, contextengine.Config{})
		h.user("request", "Fix parser.go")
		if _, _, err := h.request("t1"); err != nil {
			t.Fatal(err)
		}
		h.response("t1", repairCall("grep"))
		h.receipt("t1", "grep", "TODO parser.go", operation.StatusCompleted)
		if _, _, err := h.request("t2"); err != nil {
			t.Fatal(err)
		}
		call := repairCall("read")
		call.Data = llm.ToolCall{CallID: "read", Name: "read", Arguments: `{"path":"parser.go"}`}
		h.response("t2", repairReasoning("r2", 10<<10), call)
		h.receipt("t2", "read", strings.Repeat("func parse() {}\n", 8), operation.StatusCompleted)
		if _, _, err := h.request("t3"); err != nil {
			t.Fatal(err)
		}
		h.response("t3", repairReasoning("r3", 3<<10), repairCall("test"))
		h.receipt("t3", "test", "TestParse failed", operation.StatusFailed)
		_, wire, err := h.request("t4")
		if err != nil {
			t.Fatal(err)
		}
		assertRepairPairs(t, wire)
		if !strings.Contains(repairWireText(wire), "func parse") {
			t.Fatal("read body lost")
		}
	})
}

func TestContinuationRepairSensitiveArgumentsAreWholePortableGroup(t *testing.T) {
	h := newRepairHistory(t, contextengine.Config{})
	h.user("request", "inspect configuration")
	if _, _, err := h.request("t1"); err != nil {
		t.Fatal(err)
	}
	call := repairCall("private")
	call.Data = llm.ToolCall{CallID: "private", Name: "read", Arguments: `{"path":".codex/auth.json"}`}
	h.response("t1", repairReasoning("private-reasoning", 128), call)
	h.receipt("t1", "private", "SYNTHETIC_PRIVATE_MARKER", operation.StatusCompleted)
	_, wire, err := h.request("t2")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range wire {
		if m["type"] == "function_call" || m["type"] == "function_call_output" || m["type"] == "reasoning" {
			t.Fatal("sensitive group retained native provider state")
		}
	}
	if strings.Contains(repairWireText(wire), "SYNTHETIC_PRIVATE_MARKER") || strings.Contains(repairWireText(wire), ".codex/auth.json") {
		t.Fatal("private arguments/body leaked")
	}
}

func TestContinuationRepairResponseOrderAndUnreceivedUpdate(t *testing.T) {
	h := newRepairHistory(t, contextengine.Config{})
	h.user("request", "inspect source.go")
	if _, _, err := h.request("t1"); err != nil {
		t.Fatal(err)
	}
	h.response("t1", repairReasoning("reason-a", 128), llm.Item{Type: llm.ItemMessage, ProviderID: "commentary", Data: llm.Message{Role: llm.RoleAssistant, Phase: "commentary", Text: "Inspecting."}}, repairCall("read-a"), repairReasoning("reason-b", 128), repairCall("read-b"))
	h.receipt("t1", "read-a", "a receipt", operation.StatusCompleted)
	h.receipt("t1", "read-b", "running", operation.StatusAwaiting)
	_, wire, err := h.request("t2")
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, item := range wire {
		if id, ok := item["id"].(string); ok {
			order = append(order, id)
		}
	}
	if !slices.Equal(order, []string{"reason-a", "commentary", "fc-read-a", "reason-b", "fc-read-b"}) {
		t.Fatal("provider response order changed")
	}
	h.response("t2", llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "Waiting."}})
	h.user("new-user", "another instruction")
	h.receipt("t1", "read-b", "NEW_FINAL_RECEIPT", operation.StatusCompleted)
	for range 3 {
		built, err := h.b.Build()
		if err != nil {
			t.Fatal(err)
		}
		body, err := requestBody(built.Request, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), "NEW_FINAL_RECEIPT") {
			t.Fatal("unreceived async update lost after new user or repeated Build")
		}
	}
}

func TestContinuationRepairTrueRequiredOverflow(t *testing.T) {
	h := newRepairHistory(t, contextengine.Config{InputBudget: 5000})
	h.user("request", "inspect source.go")
	if _, _, err := h.request("t1"); err != nil {
		t.Fatal(err)
	}
	h.response("t1", repairCall("read"))
	h.receipt("t1", "read", strings.Repeat("x", 20000), operation.StatusCompleted)
	if _, err := h.b.Build(); !contextengine.IsBudgetError(err) {
		t.Fatal("true mandatory overflow did not fail locally")
	}
}

package contextbuilder

import (
	"encoding/json/v2"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

type contextFixture struct {
	b        *builder
	sequence sessionstore.Sequence
	history  []sessionstore.Item
}

func newContextFixture(t testing.TB, provider string, config contextengine.Config) *contextFixture {
	t.Helper()
	b := NewBuilder().(*builder)
	if _, err := EnableCompaction(b, config, contextengine.Constraints{ToolPolicy: "deny", Filesystem: "enforced-by-Unreal", Network: "enforced-by-Unreal", Process: "denied"}); err != nil {
		t.Fatal(err)
	}
	b.SetRuntimeProvider(provider)
	b.ConfigureRuntime(llm.Model{ID: "synthetic-model", ReasoningEffort: "medium"}, "required runtime instructions", nil, nil, provider == "claude-code", true)
	b.SetContextRuntime(sessionstore.RuntimeSelection{Version: 1, Provider: provider, Model: "synthetic-model", Effort: "medium"}, provider != "claude-code")
	return &contextFixture{b: b}
}
func (f *contextFixture) item(t testing.TB, kind sessionstore.ItemKind, data any) {
	t.Helper()
	f.sequence++
	item := sessionstore.Item{Sequence: f.sequence, RecordedAt: time.Unix(int64(f.sequence), 0).UTC(), Kind: kind, Data: data}
	f.b.SetHistoryItem(item)
	switch v := data.(type) {
	case inbox.Input:
		if v.Kind == inbox.InputExternal {
			if err := f.b.AddExternalInput(v); err != nil {
				t.Fatal(err)
			}
		} else if v.Kind == inbox.InputPeer {
			if err := f.b.AddPeerInput(v); err != nil {
				t.Fatal(err)
			}
		}
	case session.Turn:
		f.b.Commit()
	case sessionstore.ModelResponse:
		f.b.AddModelResponse(v.Response)
	case sessionstore.ToolCallStatus:
		f.b.AddToolResult(v.CallID, []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: v.Status.Error}}, false)
	}
	f.b.ObserveHistory(item)
	f.history = append(f.history, item)
}
func (f *contextFixture) user(t testing.TB, id, text string) {
	t.Helper()
	payload, _ := json.Marshal(text)
	f.item(t, sessionstore.ItemInput, inbox.Input{ID: inbox.ID(id), Kind: inbox.InputExternal, Payload: payload})
}
func (f *contextFixture) turn(t testing.TB, id, text, answer string) {
	f.user(t, id+"-input", text)
	f.item(t, sessionstore.ItemTurn, session.Turn{ID: session.TurnID(id), Type: session.TurnRegular})
	f.item(t, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: session.TurnID(id), Response: llm.Response{Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: answer}}}}})
}
func requestText(r Result) string {
	var b strings.Builder
	for _, item := range r.Request.Input {
		b.WriteString(contextengine.Text(item))
		b.WriteByte('\n')
	}
	return b.String()
}

func TestCompactionProviderSwitchNativeToolsAndPrivateReasoning(t *testing.T) {
	f := newContextFixture(t, "openai-codex", contextengine.Config{InputBudget: 12000})
	f.b.AddTool(llm.Tool{Name: "read", Type: llm.ToolFunction})
	f.user(t, "task", "Read public.txt")
	f.item(t, sessionstore.ItemTurn, session.Turn{ID: "tool-turn", Type: session.TurnRegular})
	f.item(t, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "tool-turn", Response: llm.Response{Output: []llm.Item{{Type: llm.ItemReasoning, Data: llm.Reasoning{Raw: []byte(`{"type":"reasoning","encrypted_content":"private-replay-marker"}`)}}, {ProviderID: "native-call", Type: llm.ItemToolCall, Data: llm.ToolCall{Name: "read", CallID: "call-1", Arguments: `{"path":"public.txt"}`}}}}})
	status := sessionstore.Item{Sequence: f.sequence + 1, Kind: sessionstore.ItemToolCallStatus, Data: sessionstore.ToolCallStatus{TurnID: "tool-turn", CallID: "call-1"}}
	f.b.SetHistoryItem(status)
	f.b.AddToolResult("call-1", []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "original public result"}}, false)
	f.b.ObserveHistory(status)
	before, err := f.b.Build()
	if err != nil {
		t.Fatal(err)
	}
	var calls, results, private int
	for _, i := range before.Request.Input {
		switch i.Type {
		case llm.ItemToolCall:
			calls++
		case llm.ItemToolResult:
			results++
		case llm.ItemReasoning:
			private++
		}
	}
	if calls != 1 || results != 1 || private != 1 || len(before.Request.Tools) != 1 {
		t.Fatalf("native loop lost: calls=%d results=%d private=%d", calls, results, private)
	}
	pkg, _ := json.Marshal(before.Package)
	if strings.Contains(string(pkg), "private-replay-marker") {
		t.Fatal("private replay entered index/package")
	}
	f.b.SetRuntimeProvider("claude-code")
	f.b.ConfigureRuntime(llm.Model{ID: "sonnet", ReasoningEffort: "high"}, "text only", nil, nil, true, true)
	f.b.SetContextRuntime(sessionstore.RuntimeSelection{Version: 1, Provider: "claude-code", Model: "sonnet", Effort: "high", Revision: 2, ContextWindow: 9000}, false)
	claude, err := f.b.Build()
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range claude.Request.Input {
		if i.Type != llm.ItemMessage || i.ProviderID != "" {
			t.Fatal("private/native protocol crossed providers")
		}
	}
	if strings.Contains(requestText(claude), "private-replay-marker") || len(claude.Request.Tools) != 0 || claude.Package.Runtime.Revision != 2 || claude.Package.Budget.Window != 9000 {
		t.Fatal("switch identity/capability/budget failed")
	}
	f.b.SetRuntimeProvider("openai-codex")
	f.b.ConfigureRuntime(llm.Model{ID: "gpt"}, "tools again", []llm.Tool{{Name: "read"}}, nil, false, true)
	f.b.SetContextRuntime(sessionstore.RuntimeSelection{Version: 1, Provider: "openai-codex", Model: "gpt", Effort: "medium", Revision: 3}, true)
	restored, err := f.b.Build()
	if err != nil {
		t.Fatal(err)
	}
	if len(restored.Request.Tools) != 1 {
		t.Fatal("Codex tools did not return")
	}
}

func TestCompactionSecretExclusionsDeterminismAndReplay(t *testing.T) {
	f := newContextFixture(t, "claude-code", contextengine.Config{InputBudget: 6500})
	f.turn(t, "safe", "original safe task", "safe response")
	f.turn(t, "secret", "access_token=secret-access-marker account_id=account-marker", "api_key=sk-private-marker")
	f.b.SetHistoryItem(sessionstore.Item{Kind: sessionstore.ItemModelResponse, Data: sessionstore.ModelResponse{TurnID: "private"}})
	f.b.AddModelResponse(llm.Response{Output: []llm.Item{{ProviderID: "provider-id-marker", Type: llm.ItemReasoning, Data: llm.Reasoning{Summary: []string{"private-summary-marker"}, Raw: []byte(`{"hidden":"private-state-marker"}`)}}}})
	f.user(t, "latest", "continue original safe task")
	r, err := f.b.Build()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"secret-access-marker", "sk-private-marker", "private-summary-marker", "private-state-marker", "provider-id-marker"} {
		if strings.Contains(requestText(r), s) {
			t.Fatalf("secret/private data leaked: %s", s)
		}
	}
	if r.Report.Context.Excluded != 0 || !strings.Contains(requestText(r), "account-marker") || !strings.Contains(requestText(r), "[REDACTED]") {
		t.Fatalf("exclusions=%d", r.Report.Context.Excluded)
	}
	again, err := f.b.Build()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.Package, again.Package) {
		t.Fatal("identical input yielded different package")
	}
	// Mutating a returned package must not change the engine's owned snapshot.
	r.Package.Selected[0].Unit.Source.Sequence = 999999
	again2, _ := f.b.Build()
	if !reflect.DeepEqual(again.Package, again2.Package) {
		t.Fatal("caller mutated engine package")
	}
	replay := newContextFixture(t, "claude-code", f.b.contextConfig)
	for _, item := range f.history {
		replay.item(t, item.Kind, item.Data)
	}
	resumed, err := replay.b.Build()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again.Request, resumed.Request) {
		t.Fatal("replay changed request selection")
	}
	f.user(t, "sensitive-current", "Continue the task. Authorization: Bearer private-input-marker")
	r, err = f.b.Build()
	if err != nil || strings.Contains(requestText(r), "private-input-marker") || !strings.Contains(requestText(r), "Continue the task.") || !strings.Contains(requestText(r), "[REDACTED]") {
		t.Fatal("current input must continue with only credentials redacted", err)
	}
}

func TestCompactionSecretSourceBlocksUnlabelledReceipt(t *testing.T) {
	f := newContextFixture(t, "openai-codex", contextengine.Config{})
	f.turn(t, "old", "public task", "public answer")
	f.item(t, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "old", Response: llm.Response{Output: []llm.Item{{Type: llm.ItemToolCall, Data: llm.ToolCall{Name: "read", CallID: "secret-source", Arguments: `{"path":"/synthetic/.codex/auth.json"}`}}}}})
	record := sessionstore.Item{Sequence: f.sequence + 1, Kind: sessionstore.ItemToolCallStatus, Data: sessionstore.ToolCallStatus{TurnID: "old", CallID: "secret-source"}}
	f.b.SetHistoryItem(record)
	f.b.AddToolResult("secret-source", []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "unlabelled-private-material-marker"}}, false)
	f.b.ObserveHistory(record)
	f.b.Commit()
	f.user(t, "current", "Recall the source result")
	r, err := f.b.Build()
	if err != nil {
		t.Fatal(err)
	}
	if r.Report.Context.Excluded != 2 {
		t.Fatal("secret-source call and receipt were not both excluded", r.Report.Context)
	}
	for _, s := range r.Package.Selected {
		if strings.Contains(contextengine.Text(s.Unit.Item), "unlabelled-private-material-marker") {
			t.Fatal("secret-source receipt reached context")
		}
	}
	for i := 1; i <= r.Report.Context.CanonicalUnits; i++ {
		if u, ok := f.b.engine.ResolveUnit(fmt.Sprintf("unit-%020d", i)); ok && (strings.Contains(contextengine.Text(u.Item), "unlabelled-private-material-marker") || strings.Contains(contextengine.Text(u.Item), ".codex/auth.json")) {
			t.Fatal("excluded secret-source data is recallable")
		}
	}
}

func TestCompactionNativeImageIntactOrExplicitBudgetError(t *testing.T) {
	for _, size := range []int{32, 10000} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			f := newContextFixture(t, "openai-codex", contextengine.Config{InputBudget: 6000, LargeToolTokens: 200})
			f.turn(t, "image", "inspect a public image", "starting")
			f.item(t, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "image", Response: llm.Response{Output: []llm.Item{{Type: llm.ItemToolCall, Data: llm.ToolCall{Name: "ViewImage", CallID: "image-call", Arguments: `{"path":"public.png"}`}}}}})
			f.b.SetHistoryItem(sessionstore.Item{Kind: sessionstore.ItemToolCallStatus, Data: sessionstore.ToolCallStatus{TurnID: "image", CallID: "image-call"}})
			image := "data:image/png;base64," + strings.Repeat("YQ==", size)
			f.b.AddToolResult("image-call", []llm.ToolResultOutput{{Kind: llm.ToolResultImage, Value: image}}, false)
			r, err := f.b.Build()
			if size == 10000 {
				if !contextengine.IsBudgetError(err) {
					t.Fatal("oversized current image silently lost", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, item := range r.Request.Input {
				if v, ok := item.Data.(llm.ToolResult); ok && len(v.Output) == 1 && v.Output[0].Value == image && v.Output[0].Kind == llm.ToolResultImage {
					found = true
				}
			}
			if !found {
				t.Fatal("current native image was replaced with a reference")
			}
		})
	}
}

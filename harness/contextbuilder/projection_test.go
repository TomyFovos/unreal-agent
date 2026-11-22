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
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudeagent"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

type contextFixture struct {
	b        *builder
	sequence sessionstore.Sequence
	history  []sessionstore.Item
}

func newContextFixture(t testing.TB, provider string, config contextengine.Config) *contextFixture {
	t.Helper()
	b := NewBuilder().(*builder)
	if _, err := EnableWindow(b, config, contextengine.Constraints{ToolPolicy: "deny", Filesystem: "enforced-by-Unreal", Network: "enforced-by-Unreal", Process: "denied"}); err != nil {
		t.Fatal(err)
	}
	b.SetRuntimeProvider(provider)
	if (contextengine.TokenEstimator{Provider: provider}).Claude() {
		b.SetWindowPrompt(claudeagent.WindowPromptLine)
	}
	b.ConfigureRuntime(llm.Model{ID: "synthetic-model", ReasoningEffort: "medium"}, "required runtime instructions", nil, nil, provider == "claude-code", true)
	b.SetContextRuntime(sessionstore.RuntimeSelection{Version: 1, Provider: provider, Model: "synthetic-model", Effort: "medium"}, provider != "claude-code")
	return &contextFixture{b: b}
}
func (f *contextFixture) item(t testing.TB, kind sessionstore.ItemKind, data any) {
	t.Helper()
	if turn, ok := data.(session.Turn); ok && f.b.engine != nil && turn.Type == session.TurnRegular && turn.ContextAdmission == nil {
		turn.ContextStrategy = contextengine.WindowThreeLayer
		turn.ContextAdmission = &session.ContextAdmission{HeaderFingerprint: f.b.admissionFingerprint(f.b.requestEstimate().header)}
		data = turn
	}
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

func TestNestedToolActivityIsExcludedFromLayeredWindowAdmission(t *testing.T) {
	const parent, child = "toolu_agent", "toolu_read"
	private := strings.Repeat("nested-content ", 8_000)
	for _, provider := range []string{claudeagent.ProviderID, "openai-codex"} {
		t.Run(provider, func(t *testing.T) {
			var baseline int64
			for _, format := range []string{"clean", "legacy", "structured"} {
				t.Run(format, func(t *testing.T) {
					f := newContextFixture(t, provider, contextengine.Config{InputBudget: 8_000})
					f.b.SetHistoryRuntime(claudeagent.ProviderID, "synthetic-model")
					f.user(t, "task", "Review the parser")
					f.item(t, sessionstore.ItemTurn, session.Turn{ID: "batch", Type: session.TurnRegular})
					agent := llm.Item{Type: llm.ItemToolCall, ProviderID: "claude-code/" + parent,
						Data: llm.ToolCall{CallID: parent, Name: "claude_code", Arguments: `{"tool":"Agent","input":{"description":"Review the parser"}}`}}
					if format != "clean" {
						nested := llm.Item{Type: llm.ItemToolCall, ProviderID: "claude-code/" + child,
							Data: llm.ToolCall{CallID: child, Name: "claude_code", Arguments: `{"tool":"Read","input":{"file_path":"nested-file.go"}}`}}
						output := []llm.Item{nested}
						if format == "legacy" {
							// A child PostToolBatch preceded the parent receipt in old logs.
							output = append(output, llm.Item{Type: llm.ItemMessage, ProviderID: "claude-code/msg_child/native/1", Data: llm.Message{Role: llm.RoleAssistant,
								Text: `[Claude Code Agent toolu_agent] {"model":"claude-sonnet-5-5","id":"msg_child","content":[{"type":"text","text":"` + private + `"},{"type":"tool_use","id":"toolu_read","name":"Read"}]}`}})
						} else {
							output[0].ParentCallID = parent // a delegated call the Host executed
							agent.ToolObservations = []llm.ToolObservation{
								{Type: llm.ItemMessage, ParentCallID: parent, Text: private},
								{Type: llm.ItemToolResult, ParentCallID: parent, CallID: child, Text: private},
							}
						}
						f.item(t, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "batch", Response: llm.Response{Output: output}})
						f.item(t, sessionstore.ItemToolCallStatus, sessionstore.ToolCallStatus{TurnID: "batch", CallID: child, Status: tool.CallStatus{Error: private}})
						f.item(t, sessionstore.ItemTurn, session.Turn{ID: "outer", Type: session.TurnRegular, ToolContinuation: "batch"})
					}
					f.item(t, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "outer", Response: llm.Response{Output: []llm.Item{agent}}})
					f.item(t, sessionstore.ItemToolCallStatus, sessionstore.ToolCallStatus{TurnID: "outer", CallID: parent, Status: tool.CallStatus{Error: "the final Agent result"}})
					f.user(t, "next", "Continue with the result")

					replay := newContextFixture(t, provider, f.b.contextConfig)
					replay.b.SetHistoryRuntime(claudeagent.ProviderID, "synthetic-model")
					for _, item := range f.history {
						data, err := json.Marshal(item)
						if err != nil {
							t.Fatal(err)
						}
						var restored sessionstore.Item
						if err := json.Unmarshal(data, &restored); err != nil {
							t.Fatal(err)
						}
						replay.item(t, restored.Kind, restored.Data)
					}
					for _, b := range []*builder{f.b, replay.b} {
						built, err := b.Build()
						if err != nil {
							t.Fatal(err)
						}
						text := requestText(built)
						if strings.Contains(text, "nested-") || strings.Contains(text, "[Claude Code Agent ") || !strings.Contains(text, "the final Agent result") || !strings.Contains(text, "Review the parser") {
							t.Fatalf("unexpected layered request: %.500s", text)
						}
						for _, item := range built.Request.Input {
							if item.ParentCallID != "" || len(item.ToolObservations) != 0 {
								t.Fatalf("history metadata entered model input: %+v", item)
							}
						}
						d := built.Report.Context
						if format == "clean" && baseline == 0 {
							baseline = d.InputTokens
						}
						if d.InputTokens != baseline || d.EstimatedInputTokens != baseline {
							t.Fatalf("nested activity affected admission: actual=%d estimated=%d baseline=%d", d.InputTokens, d.EstimatedInputTokens, baseline)
						}
					}
					if provider == claudeagent.ProviderID {
						var records []sessionstore.ContextRecord
						if rotated, err := f.b.PrepareToolWindow(t.Context(), windowRecorder(t, f, &records), 12_000, nil); err != nil || !rotated {
							t.Fatalf("fresh window not prepared: rotated=%v error=%v", rotated, err)
						}
						built, err := f.b.Build()
						if err != nil {
							t.Fatal(err)
						}
						prompt := claudeagent.WindowPrompt(built.Request.Input)
						if strings.Contains(prompt, "nested-") || !strings.Contains(prompt, "the final Agent result") || records[0].Window.AfterTokens != built.Report.Context.EstimatedInputTokens {
							t.Fatalf("unexpected fresh Claude window: %.500s, %+v", prompt, records[0].Window)
						}
					}
				})
			}
		})
	}
}

func TestPublicProjectionProviderSwitchNativeToolsAndPrivateReasoning(t *testing.T) {
	t.Parallel()
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
	if calls != 1 || results != 1 || private != 0 || len(before.Request.Tools) != 1 {
		t.Fatalf("native loop lost: calls=%d results=%d private=%d", calls, results, private)
	}
	pkg, _ := json.Marshal(before.Package)
	if strings.Contains(string(pkg), "private-replay-marker") {
		t.Fatal("private replay entered index/package")
	}
	f.b.SetRuntimeProvider("claude-code")
	f.b.SetWindowPrompt(claudeagent.WindowPromptLine)
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
	f.b.SetWindowPrompt(nil)
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

func TestPublicProjectionSecretExclusionsDeterminismAndReplay(t *testing.T) {
	t.Parallel()
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

func TestPublicProjectionSecretSourceBlocksUnlabelledReceipt(t *testing.T) {
	t.Parallel()
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

func TestPublicProjectionNativeImageIntactOrExplicitBudgetError(t *testing.T) {
	t.Parallel()
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

func fixtureCall(id, name, arguments string) llm.Item {
	return llm.Item{Type: llm.ItemToolCall, ProviderID: "fc-" + id, Data: llm.ToolCall{CallID: id, Name: name, Arguments: arguments}}
}

func appendFixtureRecord(t testing.TB, f *contextFixture, kind sessionstore.ItemKind, data any) {
	t.Helper()
	status, receipt := data.(sessionstore.ToolCallStatus)
	record, contextRecord := data.(sessionstore.HostRecord)
	if !receipt && !contextRecord {
		f.item(t, kind, data)
		return
	}
	f.sequence++
	item := sessionstore.Item{Sequence: f.sequence, Kind: kind, Data: data}
	f.b.SetHistoryItem(item)
	if receipt {
		state, err := operation.DecodeShellState(status.Operations[0])
		if err != nil {
			t.Fatal(err)
		}
		running := status.Operations[0].Status == operation.StatusAwaiting
		text := "running"
		if state.Result != nil {
			text = state.Result.Out
		}
		f.b.AddTypedToolResult(llm.ToolResult{CallID: status.CallID, State: llm.ToolResultSucceeded, Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: text}}}, running)
	} else if record.Context != nil {
		f.b.ApplyContext(*record.Context)
	}
	f.b.ObserveHistory(item)
	f.history = append(f.history, item)
}

func fixtureReceipt(t testing.TB, f *contextFixture, id, text string, running bool) {
	t.Helper()
	state := operation.ShellState{Input: operation.ShellInput{Shell: "/bin/bash", Command: "cat source.go", Directory: "/workspace"}, BaseDirectory: "/captures", OutPath: "/captures/" + id + "/out", ErrPath: "/captures/" + id + "/err", Result: &operation.ShellResult{Out: text}}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	op := operation.Operation{ID: operation.ID("op-" + id), Type: operation.TypeShell, Version: operation.VersionShell, Status: operation.StatusCompleted, MaxOutputLength: 40000, State: raw}
	if running {
		op.Status = operation.StatusAwaiting
	}
	status := sessionstore.ToolCallStatus{TurnID: session.TurnID(id), CallID: id, Operations: []operation.Operation{op}}
	status.Status.WaitingFor = []operation.ID{op.ID}
	appendFixtureRecord(t, f, sessionstore.ItemToolCallStatus, status)
}

func fixtureExchange(t testing.TB, f *contextFixture, id, name, args, output string) {
	t.Helper()
	f.item(t, sessionstore.ItemTurn, session.Turn{ID: session.TurnID(id), Type: session.TurnRegular})
	f.item(t, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: session.TurnID(id), Response: llm.Response{Output: []llm.Item{fixtureCall(id, name, args)}}})
	fixtureReceipt(t, f, id, output, false)
	f.item(t, sessionstore.ItemTurn, session.Turn{ID: session.TurnID(id + "-ack"), Type: session.TurnRegular})
	f.item(t, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: session.TurnID(id + "-ack"), Response: llm.Response{}})
}

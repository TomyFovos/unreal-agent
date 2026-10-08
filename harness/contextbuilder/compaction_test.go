package contextbuilder

import (
	"encoding/json/v2"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
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

func TestCompactionRetrievesExactHistoricalEvidence500And1000Turns(t *testing.T) {
	for _, n := range []int{500, 1000} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			f := newContextFixture(t, "claude-code", contextengine.Config{InputBudget: 5000, RecentReserve: 1000, CheckpointThreshold: 32})
			evidence := map[int]string{5: "The exact error is ABC-1234.", 9: "Filename: src/retained_marker.go", 13: "Function: PreserveHistoricalEvidence", 17: "User constraint: never overwrite the orchid ledger", 21: "Child result: child-old discovered the violet invariant"}
			for i := 1; i <= n; i++ {
				text := fmt.Sprintf("ordinary task %d unrelated", i)
				if old, ok := evidence[i]; ok {
					text = old
				}
				f.turn(t, fmt.Sprint(i), text, "synthetic response "+strings.Repeat("routine details ", 20))
			}
			before, _ := json.Marshal(f.history)
			for _, i := range []int{5, 9, 13, 17, 21} {
				original := evidence[i]
				f.user(t, fmt.Sprintf("recall-%d", i), "Recall the exact source for "+original)
				r, err := f.b.Build()
				if err != nil {
					t.Fatal(err)
				}
				if r.Package.EstimatedTokens > r.Package.Budget.Input || r.Report.Context.Retrieved == 0 {
					t.Fatalf("budget/retrieval: %+v", r.Report.Context)
				}
				found := false
				seen := map[string]bool{}
				for _, s := range r.Package.Selected {
					if seen[s.Unit.ID] {
						t.Fatal("duplicate selected unit")
					}
					seen[s.Unit.ID] = true
					if s.Reason == "retrieved" && contextengine.Text(s.Unit.Item) == original {
						found = true
						if s.Unit.Source.Sequence == 0 || s.Unit.Source.TurnID == "" {
							t.Fatal("missing provenance")
						}
					}
				}
				if !found {
					t.Fatalf("original turn %d evidence was not retrieved", i)
				}
				if !strings.Contains(requestText(r), "required runtime instructions") || !strings.Contains(requestText(r), "Recall the exact source") {
					t.Fatal("required instructions/current input lost")
				}
				if len(r.Request.Tools) != 0 {
					t.Fatal("Claude received tools")
				}
				f.item(t, sessionstore.ItemTurn, session.Turn{ID: session.TurnID(fmt.Sprintf("recall-turn-%d", i)), Type: session.TurnRegular})
			}
			after, _ := json.Marshal(f.history[:3*n])
			if string(before) != string(after) {
				t.Fatal("context construction changed canonical history")
			}
			r, _ := f.b.Build()
			if r.Report.Context.CheckpointBoundary == 0 || r.Report.Context.CheckpointVersion != 1 {
				t.Fatal("checkpoint was not derived")
			}
			t.Logf("turns=%d units=%d selected=%d estimate=%d budget=%d checkpoint=%d", n, r.Report.Context.CanonicalUnits, len(r.Package.Selected), r.Package.EstimatedTokens, r.Package.Budget.Input, r.Report.Context.CheckpointBoundary)
		})
	}
}

func TestCompactionBudgetRecentDedupAndRequiredOverflow(t *testing.T) {
	f := newContextFixture(t, "claude-code", contextengine.Config{InputBudget: 5000, RecentReserve: 1500})
	for i := 0; i < 40; i++ {
		f.turn(t, fmt.Sprint(i), "same earlier topic", "same answer")
	}
	f.turn(t, "recent", "LATEST RAW QUESTION", "LATEST RAW RESPONSE")
	f.user(t, "current", "CURRENT QUESTION about same earlier topic")
	r, err := f.b.Build()
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"LATEST RAW QUESTION", "LATEST RAW RESPONSE", "CURRENT QUESTION"} {
		if !strings.Contains(requestText(r), text) {
			t.Fatalf("recent raw lost: %s", text)
		}
	}
	seen := map[string]bool{}
	for _, u := range r.Package.Selected {
		if u.Reason == "retrieved" && seen[contextengine.Text(u.Unit.Item)] {
			t.Fatal("retrieval duplicates recent context")
		}
		seen[contextengine.Text(u.Unit.Item)] = true
	}
	f.user(t, "oversized", strings.Repeat("current input ", 1000))
	_, err = f.b.Build()
	if !contextengine.IsBudgetError(err) {
		t.Fatalf("oversized current input: %v", err)
	}
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
	manifest, _ := json.Marshal(f.b.engine.Manifest())
	pkg, _ := json.Marshal(before.Package)
	if strings.Contains(string(manifest), "private-replay-marker") || strings.Contains(string(pkg), "private-replay-marker") {
		t.Fatal("private replay entered index/checkpoint/package")
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

func TestCompactionLargeReceiptReferenceAndCurrentFailure(t *testing.T) {
	f := newContextFixture(t, "claude-code", contextengine.Config{InputBudget: 7000, LargeToolTokens: 800, CheckpointThreshold: 4})
	f.turn(t, "old", "build the project", "starting")
	call := llm.ToolCall{CallID: "build", Name: "Bash", Arguments: `{"command":"make check"}`}
	f.item(t, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "old", Response: llm.Response{Output: []llm.Item{{Type: llm.ItemToolCall, Data: call}}}})
	status := sessionstore.ToolCallStatus{TurnID: "old", CallID: "build", Operations: []operation.Operation{{ID: "build-op", Type: operation.TypeShell, Status: operation.StatusCompleted}}}
	record := sessionstore.Item{Sequence: f.sequence + 1, Kind: sessionstore.ItemToolCallStatus, Data: status}
	f.b.SetHistoryItem(record)
	large := "LOG_START\n" + strings.Repeat("successful build output line\n", 1200) + "\nneedle TOOL-9876 exact historical diagnostic\n" + strings.Repeat("successful build output line\n", 1200) + "\nLOG_END"
	f.b.AddToolResult("build", []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: large}}, false)
	f.b.ObserveHistory(record)
	f.b.Commit()
	f.user(t, "latest", "What did TOOL-9876 say?")
	r, err := f.b.Build()
	if err != nil {
		t.Fatal(err)
	}
	if r.Report.Context.Referenced == 0 || strings.Contains(requestText(r), large) {
		t.Fatal("large successful receipt was resent")
	}
	if !strings.Contains(requestText(r), "TOOL-9876 exact historical diagnostic") {
		t.Fatal("large historical receipt could not be recalled")
	}
	failed := sessionstore.ToolCallStatus{TurnID: "old", CallID: "failed", Operations: []operation.Operation{{ID: "failure-op", Type: operation.TypeShell, Status: operation.StatusFailed}}}
	record = sessionstore.Item{Sequence: f.sequence + 2, Kind: sessionstore.ItemToolCallStatus, Data: failed}
	f.b.SetHistoryItem(record)
	f.b.AddToolResult("failed", []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "CURRENT FAILURE ABC-FAIL remains unresolved"}}, false)
	f.b.ObserveHistory(record)
	f.b.Commit()
	for i := 0; i < 100; i++ {
		f.turn(t, fmt.Sprintf("later-%d", i), "unrelated messages", "routine reply")
	}
	f.user(t, "next", "continue the task")
	r, err = f.b.Build()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(requestText(r), "CURRENT FAILURE ABC-FAIL remains unresolved") {
		t.Fatal("unresolved failure lost outside recent suffix")
	}
}

func TestCompactionSecretExclusionsDeterminismAndReplay(t *testing.T) {
	f := newContextFixture(t, "claude-code", contextengine.Config{InputBudget: 6500, CheckpointThreshold: 4})
	f.turn(t, "safe", "original safe task", "safe response")
	f.turn(t, "secret", "access_token=secret-access-marker account_id=account-marker", "api_key=sk-private-marker")
	f.b.SetHistoryItem(sessionstore.Item{Kind: sessionstore.ItemModelResponse, Data: sessionstore.ModelResponse{TurnID: "private"}})
	f.b.AddModelResponse(llm.Response{Output: []llm.Item{{ProviderID: "provider-id-marker", Type: llm.ItemReasoning, Data: llm.Reasoning{Summary: []string{"private-summary-marker"}, Raw: []byte(`{"hidden":"private-state-marker"}`)}}}})
	f.user(t, "latest", "continue original safe task")
	r, err := f.b.Build()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"secret-access-marker", "account-marker", "sk-private-marker", "private-summary-marker", "private-state-marker", "provider-id-marker"} {
		if strings.Contains(requestText(r), s) {
			t.Fatalf("secret/private data leaked: %s", s)
		}
	}
	if r.Report.Context.Excluded != 2 {
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
	f.user(t, "sensitive-current", "Authorization: Bearer private-input-marker")
	if _, err := f.b.Build(); err == nil || strings.Contains(err.Error(), "private-input-marker") {
		t.Fatal("sensitive current input was sent/exposed")
	}
}

func TestCompactionHugeHistoricalMessageExactSlicesAndNoDuplicateBody(t *testing.T) {
	f := newContextFixture(t, "claude-code", contextengine.Config{InputBudget: 7000, RecentReserve: 1700})
	f.turn(t, "old", "initial task", "initial answer")
	original := strings.Repeat("padding ", 126) + "BOUNDARY-4321 exact original 日本語" + strings.Repeat(" long uninteresting paragraph", 1500)
	f.item(t, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "old", Response: llm.Response{Output: []llm.Item{
		{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "first output"}},
		{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: original}},
	}}})
	for i := 0; i < 60; i++ {
		f.turn(t, fmt.Sprint(i), "unrelated later subject", "unrelated later answer")
	}
	f.user(t, "current", "Find the exact BOUNDARY-4321 error.")
	r, err := f.b.Build()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, selected := range r.Package.Selected {
		u := selected.Unit
		if selected.Reason == "retrieved" && strings.Contains(contextengine.Text(u.Item), "BOUNDARY-4321 exact original 日本語") {
			found = true
			if u.ParentID == "" || u.Source.OutputIndex != 1 || u.Source.TurnID != "old" || u.Source.Sequence == 0 {
				t.Fatal("large message lost exact source provenance", u.Source)
			}
			if contextengine.Text(u.Item) != original[u.Source.StartByte:u.Source.EndByte] || !utf8.ValidString(contextengine.Text(u.Item)) {
				t.Fatal("retrieval rewrote or split the original UTF-8 text")
			}
		}
	}
	if !found || strings.Contains(requestText(r), original) || r.Package.EstimatedTokens > r.Package.Budget.Input {
		t.Fatal("huge original was lost or sent in full", r.Report.Context)
	}
	// If a smaller complete source fits, its auxiliary retrieval chunks must
	// not repeat its contents in the same request.
	g := newContextFixture(t, "claude-code", contextengine.Config{InputBudget: 12000})
	whole := strings.Repeat("smaller source details ", 100) + "WHOLE-7654"
	g.turn(t, "whole", "previous user", whole)
	g.user(t, "current", "Recall WHOLE-7654")
	r, err = g.b.Build()
	if err != nil {
		t.Fatal(err)
	}
	parents := map[string]bool{}
	for _, s := range r.Package.Selected {
		if contextengine.Text(s.Unit.Item) == whole {
			parents[s.Unit.ID] = true
		}
	}
	if len(parents) != 1 {
		t.Fatal("fitting raw original was not retained")
	}
	for _, s := range r.Package.Selected {
		if parents[s.Unit.ParentID] {
			t.Fatal("whole body duplicated by a retrieved byte range")
		}
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

func TestCompactionChildTaskPinnedWithOwnHistoryAndProviderSwitch(t *testing.T) {
	f := newContextFixture(t, "openai-codex", contextengine.Config{InputBudget: 7000, RecentReserve: 1500})
	f.b.SetTaskInput("task:child-operation")
	payload, _ := json.Marshal(inbox.PeerMessage{Version: 1, Sender: "parent", Target: "child-synthetic", Handle: "child-operation", Kind: "message", Text: "EXPLICIT CHILD TASK: investigate independent-constraint.go"})
	f.item(t, sessionstore.ItemInput, inbox.Input{ID: "task:child-operation", Kind: inbox.InputPeer, Payload: payload})
	f.item(t, sessionstore.ItemTurn, session.Turn{ID: "initial-child-turn", Type: session.TurnRegular})
	for i := 0; i < 100; i++ {
		f.turn(t, fmt.Sprint(i), "unrelated progress", "routine public result")
	}
	f.b.SetRuntimeProvider("claude-code")
	f.b.ConfigureRuntime(llm.Model{ID: "synthetic-child-claude"}, "required runtime instructions", nil, nil, true, true)
	f.b.SetContextRuntime(sessionstore.RuntimeSelection{Version: 1, Provider: "claude-code", Model: "synthetic-child-claude", Revision: 8}, false)
	f.user(t, "current", "Continue.")
	r, err := f.b.Build()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(requestText(r), "EXPLICIT CHILD TASK") || r.Package.State.Task == nil || r.Package.State.Task.Sequence != 1 || r.Package.Runtime.Revision != 8 || len(r.Request.Tools) != 0 {
		t.Fatal("child lost its own canonical task or runtime")
	}
	found := false
	for _, s := range r.Package.Selected {
		if strings.Contains(contextengine.Text(s.Unit.Item), "EXPLICIT CHILD TASK") {
			found = s.Reason == "pin" && s.Unit.Required && s.Unit.Source.OperationID == "child-operation"
		}
	}
	if !found {
		t.Fatal("delegated task was not pinned")
	}
}

func TestCompactionCurrentUserRequestSurvivesLongResponseLoop(t *testing.T) {
	f := newContextFixture(t, "openai-codex", contextengine.Config{InputBudget: 6500, RecentReserve: 1000})
	f.user(t, "active-task", "EXPLICIT CURRENT REQUEST: preserve the exact original constraints.")
	f.item(t, sessionstore.ItemTurn, session.Turn{ID: "first", Type: session.TurnRegular})
	for i := 0; i < 100; i++ {
		id := session.TurnID(fmt.Sprintf("step-%d", i))
		f.item(t, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: id, Response: llm.Response{Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: strings.Repeat("routine step output ", 30)}}}}})
		f.item(t, sessionstore.ItemTurn, session.Turn{ID: id, Type: session.TurnRegular})
	}
	r, err := f.b.Build()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(requestText(r), "EXPLICIT CURRENT REQUEST") || r.Package.State.CurrentRequest == nil || r.Package.State.CurrentRequest.Sequence != 1 {
		t.Fatal("active original request was lost outside the recent suffix")
	}
	found := false
	for _, s := range r.Package.Selected {
		if s.Unit.Source.Sequence == 1 {
			found = s.Unit.Required && s.Reason == "pin"
		}
	}
	if !found {
		t.Fatal("current request became optional historical evidence")
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

func TestCompactionReferenceDoesNotHideRetrievableSmallReceipt(t *testing.T) {
	f := newContextFixture(t, "claude-code", contextengine.Config{InputBudget: 6000, LargeToolTokens: 200})
	f.turn(t, "receipt", "public task", "starting")
	f.b.SetHistoryItem(sessionstore.Item{Sequence: f.sequence + 1, Kind: sessionstore.ItemToolCallStatus, Data: sessionstore.ToolCallStatus{TurnID: "receipt", CallID: "receipt-call"}})
	original := "SMALL-4321 " + strings.Repeat("exact original receipt ", 12)
	f.b.AddToolResult("receipt-call", []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: original}}, false)
	f.b.Commit()
	f.user(t, "current", "Recall SMALL-4321")
	r, err := f.b.Build()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range r.Package.Selected {
		if s.Reason == "retrieved" && contextengine.Text(s.Unit.Item) == original {
			found = true
		}
	}
	if !found || r.Report.Context.Referenced == 0 {
		t.Fatal("reference metadata suppressed raw evidence retrieval")
	}
}

func TestCompactionSyntheticMixedProvidersAndChildEvidence(t *testing.T) {
	for _, turns := range []int{500, 1000} {
		t.Run(fmt.Sprint(turns), func(t *testing.T) {
			f := newContextFixture(t, "claude-code", contextengine.Config{InputBudget: 7000, RecentReserve: 1500})
			const childEvidence = "EXACT-HIST-CHILD-5: child observed src/historical_child.go without mutating it."
			provider := "claude-code"
			var revision uint64
			for i := 0; i < turns; i++ {
				if i == 5 {
					payload, _ := json.Marshal(inbox.PeerMessage{Version: 1, Sender: "child-original", Target: "parent", Handle: "old-child-op", Kind: "message", Text: childEvidence})
					f.item(t, sessionstore.ItemInput, inbox.Input{ID: "child-outcome", Kind: inbox.InputPeer, Payload: payload})
				}
				if i != 0 && i%125 == 0 {
					provider = map[string]string{"claude-code": "openai-codex", "openai-codex": "claude-code"}[provider]
					revision++
					choice := sessionstore.RuntimeSelection{Version: 1, Provider: provider, Model: "synthetic-model", Effort: "medium", Revision: revision, RequestID: fmt.Sprintf("switch-%d", revision)}
					tools := []llm.Tool(nil)
					if provider == "openai-codex" {
						tools = []llm.Tool{{Name: "read", Type: llm.ToolFunction}}
					}
					f.b.SetRuntimeProvider(provider)
					f.b.ConfigureRuntime(llm.Model{ID: choice.Model, ReasoningEffort: choice.Effort}, "required runtime instructions", tools, nil, provider == "claude-code", true)
					f.b.SetContextRuntime(choice, provider != "claude-code")
					f.item(t, sessionstore.ItemHostRecord, sessionstore.HostRecord{Version: 1, Kind: sessionstore.HostRuntimeApplied, Selection: &choice})
				}
				f.turn(t, fmt.Sprint(i), "ordinary repetitive subject", strings.Repeat("ordinary repetitive response ", 20))
			}
			f.user(t, "recall-child", "Retrieve EXACT-HIST-CHILD-5 and src/historical_child.go")
			r, err := f.b.Build()
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, s := range r.Package.Selected {
				if s.Reason == "retrieved" && strings.Contains(contextengine.Text(s.Unit.Item), childEvidence) {
					found = s.Unit.Kind == contextengine.ChildResult && s.Unit.Source.ChildID == "child-original" && s.Unit.Source.OperationID == "old-child-op" && s.Unit.Source.Sequence != 0
				}
			}
			if !found || r.Package.Runtime.Provider != provider || r.Package.Runtime.Revision != revision || r.Package.EstimatedTokens > 7000 || len(r.Request.Tools) != 1 {
				t.Fatal("long mixed-provider history lost bounded child evidence/identity/capability")
			}
		})
	}
}

func BenchmarkCompactionBuild1000Turns(b *testing.B) {
	f := newContextFixture(b, "claude-code", contextengine.Config{InputBudget: 8000, RecentReserve: 2500})
	for i := 0; i < 1000; i++ {
		f.turn(b, fmt.Sprint(i), fmt.Sprintf("task %d independent symbol", i), strings.Repeat("public response ", 30))
	}
	f.user(b, "current", "Recall task 5 independent symbol")
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := f.b.Build(); err != nil {
			b.Fatal(err)
		}
	}
}

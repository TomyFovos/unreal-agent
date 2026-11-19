package coordinator

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudeagent"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

type restoreScenario struct {
	name, provider string
	historical     bool
}

var restoreScenarios = []restoreScenario{
	{contextengine.WindowThreeLayer, claudeagent.ProviderID, false},
	{"historical summary", claudeagent.ProviderID, true},
	{"historical summary", "openai-codex", true},
}

func TestCoordinatorRestoresLargeHistory(t *testing.T) {
	const limit = 6 * time.Second
	for _, tc := range restoreScenarios {
		t.Run(tc.name+"/"+tc.provider, func(t *testing.T) {
			store := largeRestoreHistory(t, tc.historical)
			ctx := t.Context()
			// Keep the latency budget on production builds. The race suite
			// exercises the same replay with instrumentation and its own timeout.
			if !raceInstrumented {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, limit)
				defer cancel()
			}
			start := time.Now()
			if err := replayLargeHistory(ctx, store, tc); err != nil {
				t.Fatal(err)
			}
			if elapsed := time.Since(start); !raceInstrumented && elapsed > limit {
				t.Fatalf("coordinator restore of %d items took %v, limit %v", len(store.items), elapsed, limit)
			}
		})
	}
}

func BenchmarkCoordinatorRestoreLargeHistory(b *testing.B) {
	for _, tc := range restoreScenarios {
		b.Run(tc.name+"/"+tc.provider, func(b *testing.B) {
			store := largeRestoreHistory(b, tc.historical)
			b.ReportAllocs()
			for b.Loop() {
				if err := replayLargeHistory(b.Context(), store, tc); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(len(store.items)), "items/op")
		})
	}
}

func replayLargeHistory(ctx context.Context, store *fakeStore, tc restoreScenario) error {
	builder := contextbuilder.NewBuilder()
	if _, err := contextbuilder.EnableWindow(builder, contextengine.Config{}, contextengine.Constraints{}); err != nil {
		return err
	}
	builder.(interface{ SetRuntimeProvider(string) }).SetRuntimeProvider(tc.provider)
	if tc.provider == claudeagent.ProviderID {
		builder.(interface{ SetWindowPrompt(llm.WindowPrompt) }).SetWindowPrompt(claudeagent.WindowPromptLine)
	}
	builder.SetModel(llm.Model{ID: "fixture"})
	registry := tool.NewRegistry(tool.StaticTranslators{ViewImage: testTranslator{}}, tool.ViewImageName)
	history := &restoreProbeStore{fakeStore: store}
	current := New(Dependencies{SessionID: "session-1", Sessions: history, ContextBuilder: builder, Tools: registry}).(*coordinator)
	return current.restore(ctx)
}

func largeRestoreHistory(t testing.TB, projectReceipts bool) *fakeStore {
	t.Helper()
	store := emptyFakeStore()
	const rounds = 15000
	const projectionInterval = 10
	itemCount := 10 * rounds
	if projectReceipts {
		itemCount += rounds/projectionInterval + 1
	}
	store.items = make([]sessionstore.Item, 0, itemCount)
	add := func(kind sessionstore.ItemKind, data any) {
		store.items = append(store.items, storedItem(sessionstore.Sequence(len(store.items)+1), kind, data))
	}
	encode := func(v any) []byte {
		t.Helper()
		payload, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return payload
	}
	user := encode("Review the work and coordinate the remaining tasks.")
	peer := encode(inbox.PeerMessage{Version: 1, Sender: "child", Target: "session-1", Handle: "worker",
		Kind: "progress", Text: "Checked the relevant files and continued the review."})
	child := encode(operation.RemoteJobState{Plan: operation.RemoteJobPlan{
		Type: "subagent", Version: 1, Data: []byte(`{"Action":"start","ChildID":"child"}`),
	}})
	for i := range rounds {
		suffix := strconv.Itoa(i)
		start, finish := session.TurnID("start-"+suffix), session.TurnID("finish-"+suffix)
		call := "call-" + suffix
		var input inbox.Input
		if i == 0 {
			input = inbox.Input{Kind: inbox.InputExternal, Payload: user}
		} else {
			input = inbox.Input{Kind: inbox.InputPeer, Payload: peer}
		}
		input.ID = inbox.ID("input-" + suffix)
		add(sessionstore.ItemInput, input)
		add(sessionstore.ItemTurn, session.Turn{ID: start, Type: session.TurnRegular})
		add(sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: start, Response: llm.Response{Output: []llm.Item{
			{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "I will check the results before continuing."}},
			{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: call, Name: tool.ViewImageName, Arguments: `{"path":"fixture.png"}`}},
		}}})
		op := operation.Operation{ID: operation.ID("child-" + suffix), Type: operation.TypeRemoteJob,
			Version: operation.VersionRemoteJob, State: child, Status: operation.StatusAwaiting}
		status := sessionstore.ToolCallStatus{TurnID: start, CallID: call, Status: tool.CallStatus{WaitingFor: []operation.ID{op.ID}}}
		status.Operations = []operation.Operation{op}
		add(sessionstore.ItemToolCallStatus, status)
		op.Status = operation.StatusCompleted
		status.Operations = []operation.Operation{op}
		add(sessionstore.ItemToolCallStatus, status)
		add(sessionstore.ItemInput, inbox.Input{ID: inbox.ID("peer-" + suffix), Kind: inbox.InputPeer, Payload: peer, Deferred: true})
		add(sessionstore.ItemHostRecord, sessionstore.HostRecord{Kind: sessionstore.HostContext,
			Context: &sessionstore.ContextRecord{Kind: "deferred_inputs"}})
		add(sessionstore.ItemTurn, session.Turn{ID: finish, PreviousTurnID: start, Type: session.TurnRegular})
		add(sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: finish, Response: llm.Response{Output: []llm.Item{
			{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "The child finished this check."}},
		}}})
		add(sessionstore.ItemHostRecord, sessionstore.HostRecord{Kind: sessionstore.HostActivity})
		if projectReceipts && i%projectionInterval == 0 {
			// Older histories recorded replacements after the receipt was sent.
			add(sessionstore.ItemHostRecord, sessionstore.HostRecord{Kind: sessionstore.HostContext, Context: &sessionstore.ContextRecord{
				Kind: "projection", UnitID: fmt.Sprintf("unit-%020d", 6*i+4), Items: []llm.Item{
					{Type: llm.ItemToolResult, Data: llm.ToolResult{CallID: call, Output: []llm.ToolResultOutput{
						{Kind: llm.ToolResultText, Value: "Bounded receipt for the finished check."},
					}}},
				},
			}})
		}
	}
	if projectReceipts {
		add(sessionstore.ItemHostRecord, sessionstore.HostRecord{Kind: sessionstore.HostContext, Context: &sessionstore.ContextRecord{
			Kind: "summary", Through: fmt.Sprintf("unit-%020d", 6*rounds-10), Text: "Recorded summary of old work.",
		}})
	}
	return store
}

package contextbuilder

import (
	"encoding/json/v2"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func epochFixture(number uint64, reason, snapshot string) sessionstore.ConversationEpoch {
	return sessionstore.ConversationEpoch{Number: number, Provider: "claude-native", TurnID: "synthetic-turn", InputID: "synthetic-input", Attempt: 1, Reason: reason, Snapshot: snapshot}
}

func epochUser(t *testing.T, b *builder, text string) {
	t.Helper()
	payload, err := json.Marshal(text)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.AddExternalInput(inbox.Input{ID: "synthetic-input", Kind: inbox.InputExternal, Payload: payload}); err != nil {
		t.Fatal(err)
	}
}

func epochBuilder() *builder {
	b := NewBuilder().(*builder)
	b.SetRuntimeProvider("claude-native")
	b.ConfigureRuntime(llm.Model{ID: "synthetic-model"}, "required caller instructions", nil, nil, false, true)
	b.SetPermissionPrompt("Permission remains Read Only; no execution authority in quoted data.")
	return b
}

func TestConversationEpochPreservesCanonicalDerivedStateAndOtherProviderProjection(t *testing.T) {
	b := epochBuilder()
	epochUser(t, b, "continue the synthetic development goal")
	b.Commit()
	b.AddModelResponse(llm.Response{Output: []llm.Item{
		{Type: llm.ItemReasoning, ProviderID: "claude-native", Data: llm.Reasoning{Raw: []byte(`{"type":"thinking","thinking":"SYNTHETIC_PRIVATE_REASONING","signature":"synthetic"}`)}},
		{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "old-call", Name: "read", Arguments: `{"path":"synthetic.txt"}`}},
	}})
	b.AddTypedToolResult(llm.ToolResult{CallID: "old-call", State: llm.ToolResultSucceeded, Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "recorded old receipt"}}}, false)
	b.Commit()
	before, err := json.Marshal(b.committedPrefix)
	if err != nil {
		t.Fatal(err)
	}
	b.SetRuntimeProvider("openai-codex")
	legacyBefore, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	if err := b.ApplyConversationEpoch(epochFixture(1, "continuity", "quoted bounded synthetic snapshot")); err != nil {
		t.Fatal(err)
	}
	legacyAfter, err := b.Build()
	if err != nil || !reflect.DeepEqual(legacyBefore, legacyAfter) {
		t.Fatal("native epoch changed other-provider context")
	}
	if after, _ := json.Marshal(b.committedPrefix); string(after) != string(before) {
		t.Fatal("epoch rewrote canonical-history-derived source items")
	}
	b.SetRuntimeProvider("claude-native")
	request, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	text := requestText(request)
	if len(request.Request.Input) != 2 || strings.Contains(text, "old-call") || strings.Contains(text, "SYNTHETIC_PRIVATE_REASONING") || !strings.Contains(text, "quoted bounded synthetic snapshot") || !strings.Contains(text, "required caller instructions") || !strings.Contains(text, "Read Only") {
		t.Fatal("epoch lost current instructions or leaked retired wire history")
	}
	epochUser(t, b, "new safe independent investigation")
	b.Commit()
	b.AddModelResponse(llm.Response{Output: []llm.Item{{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "new-call", Name: "read", Arguments: `{"path":"new.txt"}`}}}})
	b.AddTypedToolResult(llm.ToolResult{CallID: "new-call", State: llm.ToolResultFailed, Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "recorded denied receipt"}}}, false)
	request, err = b.Build()
	if err != nil || len(request.Request.Input) != 5 || typedTestResult(t, request.Request.Input, "new-call").State != llm.ToolResultFailed || !strings.Contains(requestText(request), "new safe independent investigation") {
		t.Fatal("epoch lost new input, typed receipt or its post-epoch call")
	}
}

func TestConversationStrictRestoreKeepsExactRawPrefixAndExistingEpochBase(t *testing.T) {
	for _, initialEpoch := range []bool{false, true} {
		t.Run(map[bool]string{false: "original", true: "after-epoch"}[initialEpoch], func(t *testing.T) {
			b := epochBuilder()
			epochUser(t, b, "synthetic goal")
			b.Commit()
			if initialEpoch {
				if err := b.ApplyConversationEpoch(epochFixture(1, "continuity", "base quoted snapshot")); err != nil {
					t.Fatal(err)
				}
				epochUser(t, b, "post-epoch request")
				b.Commit()
			}
			before, err := b.StrictConversationRequest()
			if err != nil {
				t.Fatal(err)
			}
			number := uint64(1)
			if initialEpoch {
				number = 2
			}
			if err := b.ApplyConversationEpoch(epochFixture(number, "strict_restore", "strict decision metadata, not a replacement prefix")); err != nil {
				t.Fatal(err)
			}
			after, err := b.Build()
			if err != nil || !reflect.DeepEqual(before, after.Request) {
				t.Fatal("strict restoration replaced or retired its exact prior raw prefix")
			}
		})
	}
}

func TestConversationResetForForkRestoresCanonicalProjectionWithoutParentEpoch(t *testing.T) {
	b := epochBuilder()
	epochUser(t, b, "inherited quoted parent task")
	b.Commit()
	before, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	if err := b.ApplyConversationEpoch(epochFixture(1, "continuity", "parent-only epoch")); err != nil {
		t.Fatal(err)
	}
	b.ResetConversationProjection()
	after, err := b.Build()
	if err != nil || !reflect.DeepEqual(before, after) || b.conversation != nil || b.conversationOffset != 0 || b.conversationSnapshot != "" {
		t.Fatal("fork reset retained parent model epoch or rewrote inherited facts")
	}
}

func TestConversationEpochOffsetTracksRemovedPendingPlaceholder(t *testing.T) {
	b := epochBuilder()
	epochUser(t, b, "initial synthetic goal")
	b.AddTypedToolResult(llm.ToolResult{CallID: "old-call", State: llm.ToolResultPending}, true)
	if err := b.ApplyConversationEpoch(epochFixture(1, "quarantine", "old uncertain work remains unknown")); err != nil {
		t.Fatal(err)
	}
	epochUser(t, b, "independent new request")
	b.AddTypedToolResult(llm.ToolResult{CallID: "old-call", State: llm.ToolResultSucceeded, Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "recorded reconciled fact"}}}, false)
	request, err := b.Build()
	if err != nil || !strings.Contains(requestText(request), "independent new request") {
		t.Fatal("removing pre-epoch pending placeholder skipped a post-epoch input")
	}
}

func TestConversationSnapshotIsBoundedPublicQuotedHistoricalEvidence(t *testing.T) {
	b := epochBuilder()
	epochUser(t, b, "Keep the synthetic goal across repeated epochs.")
	b.Commit()
	b.AddModelResponse(llm.Response{Output: []llm.Item{
		{Type: llm.ItemReasoning, Data: llm.Reasoning{Raw: []byte(`{"thinking":"SYNTHETIC_PRIVATE_REASONING"}`)}},
		{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "unverified model claim"}},
		{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "Authorization: Bearer SYNTHETIC_SECRET_CANARY"}},
	}})
	b.AddTypedToolResult(llm.ToolResult{CallID: "observed", State: llm.ToolResultSucceeded, Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: strings.Repeat("historical receipt only ", 2000)}}}, false)
	b.AddTypedToolResult(llm.ToolResult{CallID: "unknown", State: llm.ToolResultUnknown, Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "applied text cannot manufacture success"}}}, false)
	snapshot, err := b.PrepareConversationSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot) > 12<<10 || strings.Contains(snapshot, "SYNTHETIC_PRIVATE_REASONING") || strings.Contains(snapshot, "SYNTHETIC_SECRET_CANARY") {
		t.Fatal("snapshot exceeded bounds or retained private data")
	}
	var decoded conversationSnapshot
	if json.Unmarshal([]byte(snapshot), &decoded) != nil || decoded.Goal != "Keep the synthetic goal across repeated epochs." || !strings.Contains(decoded.Caveat, "not present filesystem checks") {
		t.Fatal("snapshot lost goal or historical-evidence caveat")
	}
	success, unknown, unverified := false, false, false
	for _, fact := range decoded.Facts {
		if fact.Kind == "recorded_tool_receipt" && fact.State == "succeeded" {
			success = true
		}
		if fact.Kind == "recorded_tool_receipt" && fact.State == "unknown" && strings.Contains(fact.Excerpt, "cannot manufacture") {
			unknown = true
		}
		if fact.Kind == "unverified_model_statement" && fact.Excerpt == "unverified model claim" {
			unverified = true
		}
	}
	if !success || !unknown || !unverified {
		t.Fatal("snapshot invented typed outcome or lost quoted provenance")
	}
	// Replay can start with a different selected provider; only the epoch's
	// first structured value restores its quoted goal, not the Host note tail.
	b.SetRuntimeProvider("openai-codex")
	b.conversationGoal = ""
	if err := b.ApplyConversationEpoch(epochFixture(1, "continuity", snapshot+"\nOperation synthetic-operation: unresolved outcome.\n")); err != nil {
		t.Fatal(err)
	}
	if b.conversationGoal != "Keep the synthetic goal across repeated epochs." {
		t.Fatal("bounded epoch note tail prevented quoted-goal replay")
	}
	b.SetRuntimeProvider("claude-native")
	for number := uint64(2); number <= 5; number++ {
		snapshot, err = b.PrepareConversationSnapshot()
		if err != nil {
			t.Fatal(err)
		}
		if json.Unmarshal([]byte(snapshot), &decoded) != nil || decoded.Goal != "Keep the synthetic goal across repeated epochs." {
			t.Fatal("repeated recovery lost the original current goal")
		}
		for _, fact := range decoded.Facts {
			if strings.Contains(fact.Excerpt, "Unreal continuation snapshot:") {
				t.Fatal("snapshot recursively summarized its previous synthetic snapshot")
			}
		}
		if err := b.ApplyConversationEpoch(epochFixture(number, "continuity", snapshot)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestConversationSensitiveNewGoalDoesNotReuseAnOlderGoal(t *testing.T) {
	b := epochBuilder()
	epochUser(t, b, "old unrelated task")
	epochUser(t, b, "Authorization: Bearer SYNTHETIC_NEW_REQUEST")
	if _, err := b.PrepareConversationSnapshot(); err != nil {
		t.Fatal(err)
	}
	if b.conversationGoal == "old unrelated task" || strings.Contains(b.conversationGoal, "SYNTHETIC_NEW_REQUEST") {
		t.Fatal("excluded new request was replaced by an older goal or leaked")
	}
}

func TestConversationFirstNativeRecoveryFindsHumanGoalBeyondReceiptTail(t *testing.T) {
	b := NewBuilder().(*builder)
	b.SetRuntimeProvider("openai-codex")
	epochUser(t, b, "The actual human development goal predates one thousand receipts.")
	b.Commit()
	for index := 0; index < 1000; index++ {
		b.AddTypedToolResult(llm.ToolResult{CallID: "synthetic-historical-call", State: llm.ToolResultSucceeded, Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "Historical receipt text must not become the user goal."}}}, false)
	}
	if b.conversation != nil || b.conversationGoal != "" {
		t.Fatal("ordinary legacy input eagerly constructed recovery state")
	}
	b.SetRuntimeProvider("claude-native")
	snapshot, err := b.PrepareConversationSnapshot()
	var decoded conversationSnapshot
	if err != nil || json.Unmarshal([]byte(snapshot), &decoded) != nil || decoded.Goal != "The actual human development goal predates one thousand receipts." {
		t.Fatal("first native recovery substituted a receipt for the original human goal")
	}
}

func TestConversationEpochIsInactiveForLegacyProvidersAndTracksLatestHumanGoal(t *testing.T) {
	for _, provider := range []string{"openai-codex", "claude-code"} {
		t.Run(provider, func(t *testing.T) {
			f := newContextFixture(t, provider, contextengine.Config{InputBudget: 6000})
			f.user(t, "before", "old synthetic goal")
			before, err := f.b.Build()
			if err != nil {
				t.Fatal(err)
			}
			model := before.Request.Model
			if err := f.b.ApplyConversationEpoch(epochFixture(1, "continuity", "native-only bounded evidence")); err != nil {
				t.Fatal(err)
			}
			after, err := f.b.Build()
			// Selection and budget must match exactly. Per-build retrieval
			// timings and cache diagnostics intentionally are not reproducible.
			if err != nil || !reflect.DeepEqual(before.Request, after.Request) || !reflect.DeepEqual(before.Package, after.Package) || !reflect.DeepEqual(before.Report.Changes, after.Report.Changes) {
				t.Fatal("persisted native epoch changed an unchanged legacy runtime projection or budget")
			}
			f.user(t, "after", "latest human goal in selected legacy provider")
			f.b.SetRuntimeProvider("claude-native")
			f.b.ConfigureRuntime(model, "required runtime instructions", nil, nil, false, true)
			active, err := f.b.Build()
			if err != nil || active.Request.Model != model || !strings.Contains(requestText(active), "native-only bounded evidence") || !strings.Contains(requestText(active), "latest human goal in selected legacy provider") {
				t.Fatal("native selection lost epoch, latest user input or unchanged model")
			}
			snapshot, err := f.b.PrepareConversationSnapshot()
			var decoded conversationSnapshot
			if err != nil || json.Unmarshal([]byte(snapshot), &decoded) != nil || decoded.Goal != "latest human goal in selected legacy provider" {
				t.Fatal("provider switch reused an older human goal")
			}
		})
	}
}

func TestConversationEpochContextLimitsAndRecordOrderFailClosed(t *testing.T) {
	b := epochBuilder()
	if err := b.ApplyConversationEpoch(epochFixture(1, "continuity", "bounded snapshot")); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(b.ApplyConversationEpoch(epochFixture(1, "continuity", "duplicate")), llm.ErrToolRecoveryRequired) {
		t.Fatal("replayed or out-of-order epoch accepted")
	}
	epochUser(t, b, strings.Repeat("synthetic public text ", 40000))
	_, err := b.Build()
	var continuation *llm.ContinuationError
	if !errors.As(err, &continuation) || continuation.Code != "epoch_context_limit" {
		t.Fatal("oversized post-epoch context did not request bounded recovery")
	}
	if _, err := b.PrepareConversationSnapshot(); err != nil {
		t.Fatal("bounded tail could not create a fresh recovery snapshot")
	}
	other := NewBuilder().(*builder)
	other.SetRuntimeProvider("openai-codex")
	if _, err := other.StrictConversationRequest(); !errors.Is(err, llm.ErrToolRecoveryRequired) {
		t.Fatal("strict native recovery changed another provider")
	}
}

func TestConversationEpochHonorsReserveAndProjectionBudget(t *testing.T) {
	f := newContextFixture(t, "claude-native", contextengine.Config{InputBudget: 6000})
	f.user(t, "input", "bounded synthetic goal")
	if err := f.b.ApplyConversationEpoch(epochFixture(1, "continuity", "bounded synthetic evidence")); err != nil {
		t.Fatal(err)
	}
	for _, check := range []func() error{
		func() error { _, err := f.b.BuildWithReserve(6000); return err },
		func() error {
			_, err := f.b.BuildWithProjection(llm.ContextProjection{Measure: func(llm.Request) (int64, error) { return 6001, nil }})
			return err
		},
		func() error {
			_, err := f.b.BuildWithProjection(llm.ContextProjection{SchemaReserve: 1 << 20, Measure: func(llm.Request) (int64, error) { return 1, nil }})
			return err
		},
	} {
		var continuation *llm.ContinuationError
		if !errors.As(check(), &continuation) || continuation.Code != "epoch_context_limit" {
			t.Fatal("epoch bypassed existing provider context reserve")
		}
	}
}

package contextbuilder

import (
	"context"
	"encoding/json/v2"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/contextstate"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudeagent"
	"github.com/unreallabsai/unreal-agent/harness/record"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

func TestWindowReservesAnotherWOfGrowthAfterRebuild(t *testing.T) {
	const threshold = 8_000
	f := newContextFixture(t, "openai-codex", contextengine.Config{
		WindowStrategy: contextengine.WindowThreeLayer, WindowThresholdTokens: threshold, InputBudget: 50_000,
	})
	f.turn(t, "old", "Keep working on the original request.", strings.Repeat("completed work ", 3_000))
	var records []sessionstore.ContextRecord
	persist := windowRecorder(t, f, &records)
	prepare := func() {
		t.Helper()
		if err := f.b.PrepareContext(t.Context(), nil, llm.RequestOptions{}, persist, false); err != nil {
			t.Fatal(err)
		}
	}
	prepare()
	if len(records) != 1 || records[0].Kind != sessionstore.ContextWindowBoundary || records[0].Window.AfterTokens >= threshold {
		t.Fatalf("first build did not fit: %+v", records)
	}
	base := records[0].Window.AfterTokens
	initial, err := f.b.Build()
	if err != nil {
		t.Fatal(err)
	}
	appendAnswer := func(id session.TurnID, text string) {
		t.Helper()
		f.item(t, sessionstore.ItemTurn, session.Turn{ID: id, Type: session.TurnRegular})
		f.item(t, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: id, Response: llm.Response{Output: []llm.Item{
			{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: text}},
		}}})
	}
	appendAnswer("one", "abcd")
	prepare()
	appendAnswer("below", strings.Repeat("x", 4*(threshold-2)))
	prepare()
	still, err := f.b.Build()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || still.Report.Context.EstimatedInputTokens != base+threshold-1 ||
		!reflect.DeepEqual(still.Request.Input[:len(initial.Request.Input)], initial.Request.Input) {
		t.Fatalf("growth below W replaced the prefix: %d boundaries, %d tokens", len(records), still.Report.Context.EstimatedInputTokens)
	}
	appendAnswer("equal", "z")
	prepare()
	if len(records) != 2 || !slices.Contains(records[1].Window.Causes, "threshold") ||
		records[1].Window.BeforeTokens != base+threshold || records[1].Window.AfterTokens >= threshold {
		t.Fatalf("W of growth did not trigger one rebuild: %d boundaries", len(records))
	}
}

func TestWindowRecordsRequiredMinimumWithoutDroppingInput(t *testing.T) {
	const threshold = 2_000
	f := newContextFixture(t, "openai-codex", contextengine.Config{
		WindowStrategy: contextengine.WindowThreeLayer, WindowThresholdTokens: threshold, InputBudget: 20_000,
	})
	original := "  Original request\r\n\t" + strings.Repeat("required details ", 800)
	f.user(t, "pending", original)
	var records []sessionstore.ContextRecord
	persist := windowRecorder(t, f, &records)
	for range 2 {
		if err := f.b.PrepareContext(t.Context(), nil, llm.RequestOptions{}, persist, false); err != nil {
			t.Fatal(err)
		}
	}
	if len(records) != 1 || !records[0].Window.MinimumExceedsTarget || records[0].Window.AfterTokens < threshold {
		t.Fatalf("oversized minimum was not recorded exactly once: %d boundaries", len(records))
	}
	built, err := f.b.Build()
	if err != nil {
		t.Fatal(err)
	}
	if tail := built.Request.Input[2:]; len(tail) != 1 || contextengine.Text(tail[0]) != original {
		t.Fatal("required input was shortened or moved out of the raw tail")
	}
}

func TestFirstWindowUsesRecentEventsFromActiveHumanInput(t *testing.T) {
	f := newContextFixture(t, "openai-codex", contextengine.Config{
		WindowStrategy: contextengine.WindowThreeLayer, WindowThresholdTokens: 2_000,
	})
	f.turn(t, "earlier", "older request", strings.Repeat("completed work ", 1_000))
	f.user(t, "active", "active request")
	active := uint64(f.sequence)
	state := contextstate.State{RecentEvents: []contextstate.RecentEvent{
		{Kind: "earlier", After: active - 1}, {Kind: "active input", After: active},
	}}
	f.b.SetWindowSource("fixture", func(context.Context) (contextstate.State, sessionstore.Sequence, error) {
		return state, f.sequence, nil
	})
	var records []sessionstore.ContextRecord
	if err := f.b.PrepareContext(t.Context(), nil, llm.RequestOptions{}, windowRecorder(t, f, &records), false); err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Kind != sessionstore.ContextWindowBoundary || records[0].Window.RecentEvents != 1 {
		t.Fatalf("expected one first boundary with one recent event, got %d records", len(records))
	}
	_, body, _ := strings.Cut(records[0].Text, "\n")
	var rendered contextstate.State
	if err := json.Unmarshal([]byte(body), &rendered); err != nil {
		t.Fatal(err)
	}
	request := rendered.Requests.Current["active"]
	if request.Status != contextstate.RequestActive || request.Text != "active request" ||
		len(rendered.RecentEvents) != 1 || rendered.RecentEvents[0].Kind != "active input" {
		t.Fatalf("first window lost the active input or kept earlier events: %s", body)
	}
}

func TestToolWindowCutsAutonomousTailAndKeepsInFlightBatches(t *testing.T) {
	const threshold = 8_000
	f := newContextFixture(t, claudeagent.ProviderID, contextengine.Config{
		WindowStrategy: contextengine.WindowThreeLayer, WindowThresholdTokens: threshold,
	})
	f.b.SetWindowPrompt(claudeagent.WindowPromptLine)
	f.b.SetWindowSource("fixture", func(context.Context) (contextstate.State, sessionstore.Sequence, error) {
		return contextstate.State{}, f.sequence, nil
	})
	const active = "  Continue the work\r\n\t原文\n"
	f.turn(t, "last-user", active, strings.Repeat("completed autonomous work ", 3_000))
	addToolBatch := func(turn, origin session.TurnID, call, message string) {
		t.Helper()
		f.item(t, sessionstore.ItemTurn, session.Turn{ID: turn, Type: session.TurnRegular, ToolContinuation: origin})
		f.item(t, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: turn, Response: llm.Response{Output: []llm.Item{
			{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: message}},
			{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: call, Name: "Read", Arguments: "{}"}},
		}}})
		f.item(t, sessionstore.ItemToolCallStatus, sessionstore.ToolCallStatus{TurnID: turn, CallID: call,
			Status: tool.CallStatus{Error: "fixture result " + call}})
	}
	addToolBatch("in-flight", "", "first", "first batch already delivered to Claude")
	addToolBatch("continuation", "in-flight", "second", "second batch about to cross W")
	if group := f.b.callGroups["first"]; f.b.unsettledGroups[group] != nil {
		t.Fatal("fixture did not acknowledge the first tool batch")
	}
	var records []sessionstore.ContextRecord
	project := func() error {
		payload, err := json.Marshal(inbox.PeerMessage{Version: 1, Sender: "child", Target: "fixture", Handle: "worker",
			Kind: "message", Text: "undelivered peer result"})
		if err != nil {
			return err
		}
		f.item(t, sessionstore.ItemInput, inbox.Input{Kind: inbox.InputPeer, ID: "queued", Payload: payload})
		return nil
	}
	if rotated, err := f.b.PrepareToolWindow(t.Context(), windowRecorder(t, f, &records), threshold, project); err != nil || !rotated {
		t.Fatalf("tool boundary rotated=%t err=%v", rotated, err)
	}
	built, err := f.b.Build()
	if err != nil {
		t.Fatal(err)
	}
	w := records[0].Window
	if len(records) != 1 || w.AfterTokens >= threshold || w.AfterTokens != built.Report.Context.EstimatedInputTokens ||
		!slices.Contains(w.Causes, sessionstore.WindowCauseMidTurn) {
		t.Fatalf("unexpected tool boundary: %+v", w)
	}
	var calls, results []string
	for _, item := range built.Request.Input[2:] {
		switch v := item.Data.(type) {
		case llm.ToolCall:
			calls = append(calls, v.CallID)
		case llm.ToolResult:
			results = append(results, v.CallID)
		}
	}
	if !slices.Equal(calls, []string{"first", "second"}) || !slices.Equal(results, calls) {
		t.Fatalf("split an in-flight batch: calls=%v results=%v", calls, results)
	}
	raw := claudeagent.WindowPrompt(built.Request.Input[2:])
	if strings.Contains(raw, active) || strings.Contains(raw, "completed autonomous work") ||
		!strings.Contains(raw, "first batch already delivered") || !strings.Contains(raw, "undelivered peer result") {
		t.Fatalf("wrong raw tail: %q", raw)
	}
	_, body, _ := strings.Cut(records[0].Text, "\n")
	var state contextstate.State
	if err := json.Unmarshal([]byte(body), &state); err != nil || state.Requests.Current["last-user-input"].Text != active {
		t.Fatalf("original active request was not retained in L2: %v", err)
	}
}

func windowRecorder(t *testing.T, f *contextFixture, records *[]sessionstore.ContextRecord) func(sessionstore.ContextRecord) error {
	t.Helper()
	return func(r sessionstore.ContextRecord) error {
		f.item(t, sessionstore.ItemHostRecord, sessionstore.HostRecord{Kind: sessionstore.HostContext, Context: &r})
		f.b.ApplyContext(r)
		*records = append(*records, r)
		return nil
	}
}

func TestWindowStartsAtThresholdAndKeepsItsPrefix(t *testing.T) {
	f := newContextFixture(t, "openai-codex", contextengine.Config{
		WindowStrategy: contextengine.WindowThreeLayer, L2TargetTokens: 10, RecentEventCount: 1,
	})
	l2Body := strings.Repeat("active task body ", 100)
	loc := record.Input("session", 1, record.UserMessage)
	state := contextstate.State{
		Requests: contextstate.Entries[contextstate.Request]{Current: map[string]contextstate.Request{
			"active": {ID: "active", Text: l2Body, Origin: contextstate.HumanInput, Status: contextstate.RequestActive, Locator: loc},
		}},
		RecentEvents: []contextstate.RecentEvent{
			{Kind: "older", Locator: loc},
			{Kind: "newer", Locator: loc},
		},
	}
	f.b.SetWindowSource("session", func(_ context.Context) (contextstate.State, sessionstore.Sequence, error) {
		return state, f.sequence, nil
	})
	f.turn(t, "old", "raw history outside the rebuilt window", strings.Repeat("retired work ", 600))
	const original = "  Original request\r\n\t続きの作業\n\n"
	f.turn(t, "last", original, "raw assistant tail")

	// Disable any inference adapter; entering the new window must be entirely
	// local. The exact same input is first W-1 and then W.
	estimate, _, _, _ := f.b.inputTokens()
	f.b.contextConfig.WindowThresholdTokens = estimate + 1
	var records []sessionstore.ContextRecord
	persist := func(r sessionstore.ContextRecord) error {
		f.item(t, sessionstore.ItemHostRecord, sessionstore.HostRecord{Kind: sessionstore.HostContext, Context: &r})
		f.b.ApplyContext(r)
		records = append(records, r)
		return nil
	}
	prepare := func() Result {
		t.Helper()
		if err := f.b.PrepareContext(t.Context(), nil, llm.RequestOptions{}, persist, false); err != nil {
			t.Fatal(err)
		}
		built, err := f.b.Build()
		if err != nil {
			t.Fatal(err)
		}
		return built
	}
	before := prepare()
	if len(records) != 1 || records[0].Kind != sessionstore.ContextWindowStart {
		t.Fatalf("rebuilt before W: %+v", records)
	}
	f.b.contextConfig.WindowThresholdTokens = estimate
	after := prepare()
	if len(records) != 2 || records[1].Kind != sessionstore.ContextWindowBoundary {
		t.Fatalf("did not rebuild at W: %+v", records)
	}
	w := records[1].Window
	if w.BeforeTokens != estimate || w.AfterTokens != after.Report.Context.InputTokens || w.BeforeTokens <= w.AfterTokens ||
		w.L2Tokens <= w.L2TargetTokens || w.RecentEvents != 1 || !slices.Equal(w.Causes, []string{"threshold"}) {
		t.Fatalf("unexpected boundary measurement: %+v", w)
	}
	if w.Tail == nil || w.Tail.Items != 2 || w.Tail.Tokens <= 0 || w.Tail.Previous == nil || !slices.Contains(*w.Tail.Previous, loc) {
		t.Fatalf("missing rendered tail size or previous raw evidence: %+v", w.Tail)
	}
	items := after.Request.Input
	if len(items) != 4 || !reflect.DeepEqual(items[0], before.Request.Input[0]) ||
		contextengine.Text(items[2]) != original || contextengine.Text(items[3]) != "raw assistant tail" {
		t.Fatalf("rebuilt input has incorrect raw messages: %s", requestText(after))
	}
	snapshot := contextengine.Text(items[1])
	_, raw, _ := strings.Cut(snapshot, "\n")
	var got contextstate.State
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	if got.Requests.Current["active"].Text != l2Body || got.Requests.Current["active"].Locator != loc ||
		len(got.RecentEvents) != 1 || got.RecentEvents[0].Kind != "newer" {
		t.Fatalf("state copy changed or lost evidence: %s", raw)
	}
	state.Requests.Current["active"] = contextstate.Request{ID: "active", Status: contextstate.RequestActive, Text: "state changed in this window"}
	f.user(t, "additional", "more raw input")
	next := prepare()
	if len(records) != 2 || !reflect.DeepEqual(next.Request.Input[:len(items)], items) ||
		contextengine.Text(next.Request.Input[len(items)]) != "more raw input" {
		t.Fatalf("window did not append to its original prefix: %s", requestText(next))
	}
}

func TestWindowFallbackRunsSummaryForOneRequest(t *testing.T) {
	config := contextengine.Config{
		WindowStrategy: contextengine.WindowThreeLayer, WindowThresholdTokens: 2_000, FallbackWindow: 20_000, InputBudget: 12_000,
	}
	f := newContextFixture(t, "openai-codex", config)
	for _, id := range []string{"old-1", "old-2", "old-3", "old-4"} {
		f.turn(t, id, "old request", strings.Repeat("recorded work ", 1_000))
	}
	f.user(t, "current", "preserve the active request")
	state := contextstate.State{Rules: map[string]contextstate.Rule{
		"rule": {Excerpt: strings.Repeat("oversized state ", 4_000)},
	}}
	f.b.SetWindowSource("fixture", func(context.Context) (contextstate.State, sessionstore.Sequence, error) {
		return state, f.sequence, nil
	})
	var records []sessionstore.ContextRecord
	persist := windowRecorder(t, f, &records)
	summaries := 0
	adapter := summaryAdapter(func(req llm.Request) llm.Response {
		summaries++
		if req.PublicHistory || req.Window != nil || contextengine.Text(req.Input[len(req.Input)-1]) != summaryInstruction {
			t.Fatalf("fallback did not use the summary request: %+v", req.Window)
		}
		return llm.Response{Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "old work summary"}}}}
	})
	if err := f.b.PrepareContext(t.Context(), adapter, llm.RequestOptions{}, persist, false); err != nil {
		t.Fatal(err)
	}
	built, err := f.b.Build()
	if err != nil {
		t.Fatal(err)
	}
	text := requestText(built)
	if summaries != 1 || built.Request.PublicHistory || built.Request.Window != nil || f.b.ContextStrategy() != contextengine.WindowSummary ||
		!strings.Contains(text, "old work summary") || !strings.Contains(text, "preserve the active request") || strings.Contains(text, "oversized state") {
		t.Fatal("fallback did not admit the original request through summary")
	}
	fallback := records[0]
	w := fallback.Window
	if fallback.Kind != sessionstore.ContextWindowFallback || fallback.Text != "" || w == nil ||
		w.Fallback != sessionstore.WindowFallbackL2OverBudget || w.InputBudgetTokens != config.InputBudget ||
		w.L2Tokens <= config.InputBudget || w.Tail == nil || w.Tail.Items != 1 ||
		w.AfterTokens != *w.HeaderTokens+w.L2Tokens+w.Tail.Tokens ||
		records[len(records)-1].Kind != "summary" {
		t.Fatalf("fallback diagnostics do not describe the rejected projection: %+v", w)
	}

	replay := newContextFixture(t, "openai-codex", config)
	for _, item := range f.history {
		replay.item(t, item.Kind, item.Data)
		if r, ok := item.Data.(sessionstore.HostRecord); ok {
			replay.b.ApplyContext(*r.Context)
		}
	}
	restored, err := replay.b.Build()
	if err != nil || !reflect.DeepEqual(built.Request.Input, restored.Request.Input) || restored.Request.PublicHistory {
		t.Fatal("replay changed the fallback request", err)
	}
	f.b.SetHistoryItem(sessionstore.Item{Data: session.Turn{ID: "summary-turn", Type: session.TurnRegular, ContextStrategy: contextengine.WindowSummary}})
	f.b.Commit()
	state.Rules = nil
	f.user(t, "next", "try the layered window again")
	if err := f.b.PrepareContext(t.Context(), nil, llm.RequestOptions{}, persist, false); err != nil {
		t.Fatal(err)
	}
	next, err := f.b.Build()
	if err != nil || !next.Request.PublicHistory || records[len(records)-1].Kind != sessionstore.ContextWindowBoundary ||
		f.b.ContextStrategy() != contextengine.WindowThreeLayer || !strings.Contains(requestText(next), "try the layered window again") {
		t.Fatal("next request did not rebuild a three-layer window", err)
	}
}

func TestToolWindowFallbackContinuesWithPrivateSummaryMode(t *testing.T) {
	f := newContextFixture(t, claudeagent.ProviderID, contextengine.Config{
		WindowStrategy: contextengine.WindowThreeLayer, WindowThresholdTokens: 2_000, InputBudget: 4_000,
	})
	f.b.SetWindowPrompt(claudeagent.WindowPromptLine)
	f.user(t, "active", "Keep working")
	f.item(t, sessionstore.ItemTurn, session.Turn{ID: "in-flight", Type: session.TurnRegular})
	f.b.SetWindowSource("fixture", func(context.Context) (contextstate.State, sessionstore.Sequence, error) {
		return contextstate.State{Rules: map[string]contextstate.Rule{
			"rule": {Excerpt: strings.Repeat("oversized state ", 2_000)},
		}}, f.sequence, nil
	})
	var records []sessionstore.ContextRecord
	persist := windowRecorder(t, f, &records)
	rotated, err := f.b.PrepareToolWindow(t.Context(), persist, 5_000, nil)
	if err != nil || !rotated || len(records) != 1 || records[0].Kind != sessionstore.ContextWindowFallback ||
		!slices.Contains(records[0].Window.Causes, sessionstore.WindowCauseMidTurn) {
		t.Fatalf("oversized tool window did not hand off the turn: rotated=%v err=%v", rotated, err)
	}
	if err := f.b.PrepareContext(t.Context(), nil, llm.RequestOptions{}, persist, false); err != nil {
		t.Fatal(err)
	}
	built, err := f.b.Build()
	if err != nil || len(records) != 1 || built.Request.Window != nil || built.Request.PublicHistory ||
		f.b.ContextStrategy() != contextengine.WindowSummary || !strings.Contains(requestText(built), "Keep working") {
		t.Fatal("private adapter did not receive the summary continuation", err)
	}
}

func TestWindowDefaultRoutesAndOptions(t *testing.T) {
	for _, tc := range []struct {
		provider, strategy string
		active             bool
	}{
		{provider: "openai-codex", active: true},
		{provider: "opencode-go", active: true},
		{provider: "openai", active: true},
		{provider: "openrouter", active: true},
		{provider: "fireworks", active: true},
		{provider: "ollama", active: true},
		{provider: "claude-code"},
		{provider: "openai-codex", strategy: contextengine.WindowSummary},
	} {
		b := NewBuilder().(*builder)
		if _, err := EnableWindow(b, contextengine.Config{WindowStrategy: tc.strategy}, contextengine.Constraints{}); err != nil {
			t.Fatal(err)
		}
		b.SetRuntimeProvider(tc.provider)
		if b.UsesThreeLayerContext() != tc.active || b.contextConfig.WindowThresholdTokens != 200_000 ||
			b.contextConfig.L2TargetTokens != 5000 {
			t.Errorf("window defaults for %s/%s: %+v", tc.provider, tc.strategy, b.contextConfig)
		}
	}
}

func TestWindowKeepsInputsPendingBeforeLastUserMessage(t *testing.T) {
	f := newContextFixture(t, "openai-codex", contextengine.Config{
		WindowStrategy: contextengine.WindowThreeLayer, WindowThresholdTokens: 2000,
	})
	f.turn(t, "old", "Raw old input", strings.Repeat("worked on the old request. ", 400))
	f.user(t, "first-pending", "First queued user input")
	f.user(t, "next-pending", "Second queued user input")
	if err := f.b.PrepareContext(t.Context(), nil, llm.RequestOptions{}, func(r sessionstore.ContextRecord) error {
		f.b.ApplyContext(r)
		return nil
	}, false); err != nil {
		t.Fatal(err)
	}
	built, err := f.b.Build()
	if err != nil {
		t.Fatal(err)
	}
	tail := built.Request.Input[2:]
	if len(tail) != 2 || contextengine.Text(tail[0]) != "First queued user input" || contextengine.Text(tail[1]) != "Second queued user input" {
		t.Fatalf("queued input was lost or reordered: %s", requestText(built))
	}
}

func TestToolWindowUsesStrictInputBudget(t *testing.T) {
	f := newContextFixture(t, "claude", contextengine.Config{
		WindowStrategy: contextengine.WindowThreeLayer, WindowThresholdTokens: 200, InputBudget: 100,
	})
	f.b.SetWindowPrompt(func(llm.Item) (string, bool) { return "projected window", true })
	var records []sessionstore.ContextRecord
	persist := func(r sessionstore.ContextRecord) error {
		records = append(records, r)
		return nil
	}
	if rotated, err := f.b.PrepareToolWindow(t.Context(), persist, 100, nil); err != nil || rotated || len(records) != 0 {
		t.Fatalf("at the input budget: rotated=%t records=%d error=%v", rotated, len(records), err)
	}
	if rotated, err := f.b.PrepareToolWindow(t.Context(), persist, 101, nil); err != nil || !rotated || len(records) != 1 {
		t.Fatalf("over the input budget: rotated=%t records=%d error=%v", rotated, len(records), err)
	}
	w := records[0].Window
	if w.BeforeTokens != 101 || !slices.Equal(w.Causes, []string{"admission", sessionstore.WindowCauseMidTurn}) {
		t.Fatalf("unexpected boundary: %+v", w)
	}
}

func TestWindowKeepsRunningToolGroupAcrossRawCut(t *testing.T) {
	f := newContextFixture(t, "openai-codex", contextengine.Config{
		WindowStrategy: contextengine.WindowThreeLayer, WindowThresholdTokens: 2000,
	})
	f.turn(t, "old", "Raw old input", strings.Repeat("worked on the old request. ", 400))
	f.user(t, "asked", "Start a child")
	f.item(t, sessionstore.ItemTurn, session.Turn{ID: "call", Type: session.TurnRegular})
	f.item(t, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "call", Response: llm.Response{
		Output: []llm.Item{
			{Type: llm.ItemReasoning, Data: llm.Reasoning{Raw: []byte(`{"type":"reasoning","encrypted_content":"opaque"}`)}},
			{Type: llm.ItemToolCall, ProviderID: "provider-item-id", Data: llm.ToolCall{CallID: "working", Name: "SubagentStart", Arguments: "{}"}},
		},
	}})
	f.b.AddToolResult("working", nil, true)
	f.user(t, "after-call", "Check other work")
	if err := f.b.PrepareContext(t.Context(), nil, llm.RequestOptions{}, func(r sessionstore.ContextRecord) error {
		f.item(t, sessionstore.ItemHostRecord, sessionstore.HostRecord{Kind: sessionstore.HostContext, Context: &r})
		f.b.ApplyContext(r)
		return nil
	}, false); err != nil {
		t.Fatal(err)
	}
	got, err := f.b.Build()
	if err != nil {
		t.Fatal(err)
	}
	tail := got.Request.Input[2:]
	if len(tail) != 3 || tail[0].Type != llm.ItemToolCall || tail[0].ProviderID != "" ||
		contextengine.Text(tail[1]) != "Check other work" || tail[2].Type != llm.ItemToolResult {
		t.Fatalf("incorrect continuation after boundary: %s", requestText(got))
	}
	if call, result := tail[0].Data.(llm.ToolCall), tail[2].Data.(llm.ToolResult); call.CallID != result.CallID ||
		contextengine.Text(tail[2]) != ToolCallRunningPayload {
		t.Fatalf("lost pending call/receipt identity: %+v, %+v", call, result)
	}
}

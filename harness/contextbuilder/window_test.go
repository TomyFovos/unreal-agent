package contextbuilder

import (
	"context"
	"encoding/json/v2"
	"fmt"
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
		WindowThresholdTokens: threshold, L2TargetTokens: threshold / 2, InputBudget: 50_000,
	})
	f.turn(t, "old", "Keep working on the original request.", strings.Repeat("completed work ", 3_000))
	var records []sessionstore.ContextRecord
	persist := windowRecorder(t, f, &records)
	prepare := func() {
		t.Helper()
		if err := f.b.PrepareContext(t.Context(), persist, false); err != nil {
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
		WindowThresholdTokens: threshold, L2TargetTokens: threshold / 2, InputBudget: 20_000,
	})
	original := "  Original request\r\n\t" + strings.Repeat("required details ", 800)
	f.user(t, "pending", original)
	var records []sessionstore.ContextRecord
	persist := windowRecorder(t, f, &records)
	for range 2 {
		if err := f.b.PrepareContext(t.Context(), persist, false); err != nil {
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
		WindowThresholdTokens: 2_000, L2TargetTokens: 1_000,
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
	if err := f.b.PrepareContext(t.Context(), windowRecorder(t, f, &records), false); err != nil {
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
		WindowThresholdTokens: threshold, L2TargetTokens: threshold / 2,
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

func TestToolWindowRetainsHeadroomAsSessionStateGrows(t *testing.T) {
	const threshold, budget = 200_000, 266_880
	f := newContextFixture(t, claudeagent.ProviderID, contextengine.Config{InputBudget: budget})
	var projection contextstate.Projection
	f.b.SetWindowSource("fixture", func(context.Context) (contextstate.State, sessionstore.Sequence, error) {
		return projection.State, f.sequence, nil
	})
	const active = "Continue reviewing the current changes."
	f.user(t, "active", active)
	var previous []sessionstore.ContextRecord
	if err := f.b.PrepareContext(t.Context(), windowRecorder(t, f, &previous), true); err != nil {
		t.Fatal(err)
	}
	addState := func(kind contextstate.EventKind, id string, value any) record.Locator {
		t.Helper()
		event, err := contextstate.NewEvent(id, kind, value)
		if err != nil {
			t.Fatal(err)
		}
		event.Record = record.Item("fixture", uint64(f.sequence+1))
		f.item(t, sessionstore.ItemContextEvent, event)
		if err := projection.Apply(event); err != nil {
			t.Fatal(err)
		}
		return event.Record
	}
	for i := range 153 {
		id := fmt.Sprintf("decision-%03d", i)
		addState(contextstate.DecisionRecorded, id, contextstate.Decision{
			ID: id, Decision: "Keep the verified implementation", Reason: strings.Repeat("recorded rationale ", 105),
		})
	}
	for i := range 32 {
		id := fmt.Sprintf("worker-%02d", i)
		report := addState(contextstate.RecentObserved, id+"-report", contextstate.RecentEvent{
			Kind: "report", Handle: id, FirstLine: "Completed review with test results",
		})
		addState(contextstate.ChildChanged, id, contextstate.ChildUpdate{Child: contextstate.Child{
			Handle: id, State: contextstate.ChildFinished, Report: report, ResultFirstLine: strings.Repeat("verified findings ", 40),
		}})
	}
	addState(contextstate.ChildChanged, "live", contextstate.ChildUpdate{Child: contextstate.Child{
		Handle: "live", State: contextstate.ChildRunning, Title: "Investigate the pending report",
	}})
	f.item(t, sessionstore.ItemTurn, session.Turn{ID: "origin", Type: session.TurnRegular})
	batch := func(id, origin session.TurnID, call, text, result string) {
		t.Helper()
		f.item(t, sessionstore.ItemTurn, session.Turn{ID: id, Type: session.TurnRegular, ToolContinuation: origin})
		var output []llm.Item
		if text != "" {
			output = append(output, llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: text}})
		}
		output = append(output, fixtureCall(call, "Read", "{}"))
		f.item(t, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: id, Response: llm.Response{Output: output}})
		f.item(t, sessionstore.ItemToolCallStatus, sessionstore.ToolCallStatus{
			TurnID: id, CallID: call, Status: tool.CallStatus{Error: result},
		})
	}
	const settled = "Previously delivered tool receipt"
	batch("settled", "origin", "old", settled+strings.Repeat(" preceding tool analysis", 20_000), "old tool receipt")
	batch("crossing", "origin", "new", "", "current tool receipt")

	var records []sessionstore.ContextRecord
	persist := windowRecorder(t, f, &records)
	prepare := func(input int64, queued func() error) bool {
		t.Helper()
		rotated, err := f.b.PrepareToolWindow(t.Context(), persist, input, queued)
		if err != nil {
			t.Fatal(err)
		}
		return rotated
	}
	const queued = "Next instruction received during the batch."
	if !prepare(267_649, func() error { f.user(t, "queued", queued); return nil }) || len(records) != 1 {
		t.Fatalf("admission should create one boundary, got %d", len(records))
	}
	w := records[0].Window
	built, err := f.b.Build()
	if err != nil {
		t.Fatal(err)
	}
	if w.L2Tokens > 5_000 || w.AfterTokens > threshold/2 || w.AfterTokens != built.Report.Context.EstimatedInputTokens ||
		!slices.Equal(w.Causes, []string{"admission", sessionstore.WindowCauseMidTurn}) {
		t.Fatalf("unexpected boundary: %+v", w)
	}
	heading, body, _ := strings.Cut(records[0].Text, "\n")
	var state contextstate.State
	if err := json.Unmarshal([]byte(body), &state); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(heading, "rec:fixture/context/state") || state.Children.Current["live"].State != contextstate.ChildRunning ||
		state.Requests.Current["active"].Text != active || len(projection.State.Decisions) != 153 || len(projection.State.Children.Current) != 33 {
		t.Fatal("the projected state lost live work or changed its source")
	}
	raw := claudeagent.WindowPrompt(built.Request.Input[2:])
	if strings.Contains(raw, settled) || !strings.Contains(raw, "current tool receipt") || !strings.Contains(raw, queued) ||
		!slices.ContainsFunc(built.Request.Input[2:], func(item llm.Item) bool {
			call, ok := item.Data.(llm.ToolCall)
			return ok && call.CallID == "new"
		}) {
		t.Fatal("the fresh window lost the crossing batch or queued input, or repeated the settled batch")
	}

	f.item(t, sessionstore.ItemTurn, session.Turn{ID: "fresh", Type: session.TurnRegular})
	// Approximate the fresh Claude input from the prompt and keep adding the
	// measured-size increments of several tool rounds in the same window.
	providerInput := 9_806 + w.AfterTokens*3/2
	for i, delta := range []int64{13_863, 22_000, 7_351, 25_000, 38_000} {
		id := fmt.Sprintf("round-%d", i)
		batch(session.TurnID(id), "fresh", id, "", strings.Repeat("next tool output ", 120))
		addState(contextstate.DecisionRecorded, id, contextstate.Decision{ID: id, Decision: strings.Repeat("new decision ", 100)})
		providerInput += delta
		if prepare(providerInput, nil) {
			t.Fatalf("rotated again after only %d tool rounds at %d input tokens", i+1, providerInput)
		}
	}
	if !prepare(budget+1, nil) || len(records) != 2 || records[1].Window.AfterTokens > threshold/2 ||
		records[1].Window.L2Tokens > 5_000 || records[1].Through < records[0].Through {
		t.Fatalf("expected the next rotation at admission with a bounded projection, got %d boundaries", len(records))
	}
}

func windowRecorder(t *testing.T, f *contextFixture, records *[]sessionstore.ContextRecord) func(sessionstore.ContextRecord) error {
	t.Helper()
	return func(r sessionstore.ContextRecord) error {
		f.item(t, sessionstore.ItemHostRecord, sessionstore.HostRecord{Kind: sessionstore.HostContext, Context: &r})
		f.b.ApplyContext(r)
		if r.Kind != sessionstore.ContextPolicyChanged {
			*records = append(*records, r)
		}
		return nil
	}
}

func TestWindowStartsAtThresholdAndKeepsItsPrefix(t *testing.T) {
	f := newContextFixture(t, "openai-codex", contextengine.Config{
		L2TargetTokens: 10, RecentEventCount: 1,
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
	persist := windowRecorder(t, f, &records)
	prepare := func() Result {
		t.Helper()
		if err := f.b.PrepareContext(t.Context(), persist, false); err != nil {
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

func TestWindowExcerptsRequiredTailAndReplaysExactly(t *testing.T) {
	for _, provider := range []string{"openai-codex", claudeagent.ProviderID, claudeagent.APIProviderID} {
		t.Run(provider, func(t *testing.T) {
			const budget = 4_000
			config := contextengine.Config{WindowThresholdTokens: 10_000, InputBudget: budget}
			f := newContextFixture(t, provider, config)
			// These are canonical tool results, with a private-conversation
			// renderer for both Claude connections.
			f.b.textOnly = false
			f.b.SetWindowSource("fixture", nil)
			f.user(t, "active", "Finish the current task.")
			f.item(t, sessionstore.ItemTurn, session.Turn{ID: "in-flight", Type: session.TurnRegular})
			message := "MESSAGE HEAD " + strings.Repeat("会話の本文", 5_000) + " MESSAGE TAIL"
			output := "OUTPUT HEAD " + strings.Repeat("ツール出力", 5_000) + " OUTPUT TAIL"
			f.item(t, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "in-flight", Response: llm.Response{Output: []llm.Item{
				{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: message}},
				fixtureCall("read", "Read", "{}"),
			}}})
			f.item(t, sessionstore.ItemToolCallStatus, sessionstore.ToolCallStatus{
				TurnID: "in-flight", CallID: "read", Status: tool.CallStatus{Error: output},
			})

			var records []sessionstore.ContextRecord
			if err := f.b.PrepareContext(t.Context(), windowRecorder(t, f, &records), false); err != nil {
				t.Fatal(err)
			}
			if len(records) != 1 || records[0].Kind != sessionstore.ContextWindowBoundary {
				t.Fatalf("expected a single rebuild, got %d records", len(records))
			}
			w := records[0].Window
			if w.Fit != sessionstore.WindowFitExcerpted || w.FitExcerptChars == nil || *w.FitExcerptChars < 30 ||
				w.RequiredInputTokens <= budget || w.InputBudgetTokens != budget ||
				w.RequiredInputTokens != *w.HeaderTokens+w.L2Tokens+w.RequiredTailTokens ||
				w.AfterTokens != *w.HeaderTokens+w.L2Tokens+w.Tail.Tokens ||
				w.AfterTokens > budget || w.Tail.Tokens >= w.RequiredTailTokens {
				t.Fatalf("incorrect fit measurements: %+v", w)
			}
			built, err := f.b.Build()
			if err != nil || built.Report.Context.InputTokens != w.AfterTokens {
				t.Fatalf("fitted projection did not match admission: %v", err)
			}
			text := requestText(built)
			for _, want := range []string{"Finish the current task.", "MESSAGE HEAD", "MESSAGE TAIL", "OUTPUT HEAD", "OUTPUT TAIL",
				"rec:fixture/3/message/0", "rec:fixture/4/tool-error", "bytes truncated"} {
				if !strings.Contains(text, want) {
					t.Fatalf("excerpt lost %q", want)
				}
			}
			if strings.Contains(text, message) || strings.Contains(text, output) {
				t.Fatal("request sent the unbounded tail")
			}
			read := f.b.engine.Units()
			if contextengine.Text(read[1].Item) != message || contextengine.Text(read[3].Item) != output {
				t.Fatal("excerpts changed the originals")
			}
			replay := newContextFixture(t, provider, config)
			replay.b.textOnly = false
			replay.b.SetWindowSource("fixture", nil)
			for _, item := range f.history {
				replay.item(t, item.Kind, item.Data)
				if r, ok := item.Data.(sessionstore.HostRecord); ok {
					raw, err := json.Marshal(r.Context)
					if err != nil {
						t.Fatal(err)
					}
					var window sessionstore.ContextRecord
					if err := json.Unmarshal(raw, &window); err != nil {
						t.Fatal(err)
					}
					replay.b.ApplyContext(window)
				}
			}
			restored, err := replay.b.Build()
			if err != nil || !reflect.DeepEqual(restored.Request, built.Request) {
				t.Fatalf("recorded excerpts changed after restore: %v", err)
			}
			// The cap only applies to units that existed at the boundary.
			followup := strings.Repeat("new input ", 1_000)
			replay.user(t, "followup", followup)
			unit, ok := replay.b.engine.ResolveUnit(replay.b.lastUnit)
			if !ok || contextengine.Text(replay.b.deriveUnit(unit)[0]) != followup {
				t.Fatal("the fit clipped an input appended after the boundary")
			}
		})
	}
}

func TestToolWindowReportsUnfittableL2WithoutCreatingBoundary(t *testing.T) {
	f := newContextFixture(t, claudeagent.ProviderID, contextengine.Config{
		WindowThresholdTokens: 2_000, L2TargetTokens: 1_000, InputBudget: 4_000,
	})
	f.user(t, "active", "Keep working")
	f.item(t, sessionstore.ItemTurn, session.Turn{ID: "in-flight", Type: session.TurnRegular})
	f.b.SetWindowSource("fixture", func(context.Context) (contextstate.State, sessionstore.Sequence, error) {
		return contextstate.State{Rules: map[string]contextstate.Rule{
			"rule": {Excerpt: strings.Repeat("oversized state ", 2_000)},
		}}, f.sequence, nil
	})
	var records []sessionstore.ContextRecord
	rotated, err := f.b.PrepareToolWindow(t.Context(), windowRecorder(t, f, &records), 5_000, nil)
	if rotated || len(records) != 0 || !contextengine.IsBudgetError(err) {
		t.Fatalf("unfittable state was admitted: rotated=%v records=%d error=%v", rotated, len(records), err)
	}
	for _, size := range []string{"input budget 4000 tokens", "system and tools", "L2", "required L1 tail", "smallest L1 tail", "minimum total"} {
		if !strings.Contains(err.Error(), size) {
			t.Fatalf("missing %q in %v", size, err)
		}
	}
}

func TestToolWindowFitsHeadroomAndKeepsQueuedInputVerbatim(t *testing.T) {
	const threshold, budget = 4_000, 20_000
	f := newContextFixture(t, claudeagent.ProviderID, contextengine.Config{
		WindowThresholdTokens: threshold, L2TargetTokens: threshold / 2, InputBudget: budget,
	})
	f.b.SetWindowSource("fixture", nil)
	f.user(t, "active", "Review the operation result.")
	f.item(t, sessionstore.ItemTurn, session.Turn{ID: "in-flight", Type: session.TurnRegular})
	f.item(t, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "in-flight", Response: llm.Response{Output: []llm.Item{
		{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: strings.Repeat("recorded assistant text ", 650)}},
		fixtureCall("read", "Read", "{}"),
	}}})
	f.item(t, sessionstore.ItemToolCallStatus, sessionstore.ToolCallStatus{
		TurnID: "in-flight", CallID: "read", Status: tool.CallStatus{Error: "tool result"},
	})
	queued := "New instruction.\n" + strings.Repeat("new details ", 170)
	var records []sessionstore.ContextRecord
	rotated, err := f.b.PrepareToolWindow(t.Context(), windowRecorder(t, f, &records), threshold, func() error {
		f.user(t, "queued", queued)
		return nil
	})
	if err != nil || !rotated || len(records) != 1 {
		t.Fatalf("tool boundary rotated=%t with %d records: %v", rotated, len(records), err)
	}
	w := records[0].Window
	result, err := f.b.Build()
	if err != nil {
		t.Fatal(err)
	}
	if !w.FitPreserveInputs || w.FitExcerptChars == nil || *w.FitExcerptChars >= len(queued) ||
		w.RequiredInputTokens >= budget || w.AfterTokens > threshold/2 || w.AfterTokens != result.Report.Context.EstimatedInputTokens {
		t.Fatalf("unexpected headroom fit: %+v", w)
	}
	if contextengine.Text(result.Request.Input[len(result.Request.Input)-1]) != queued ||
		!strings.Contains(requestText(result), "[Full message via record show: rec:fixture/3/message/0]") {
		t.Fatal("the headroom fit shortened the new input or lost the assistant message locator")
	}
}

func TestWindowDefaultRoutesAndOptions(t *testing.T) {
	for _, provider := range []string{"openai-codex", "opencode-go", "openai", "openrouter", "fireworks", "ollama", claudeagent.ProviderID, claudeagent.APIProviderID} {
		b := newContextFixture(t, provider, contextengine.Config{}).b
		if !b.UsesThreeLayerContext() || b.ContextStrategy() != contextengine.WindowThreeLayer ||
			b.contextConfig.WindowThresholdTokens != 200_000 || b.contextConfig.L2TargetTokens != 5000 {
			t.Errorf("window defaults for %s: %+v", provider, b.contextConfig)
		}
	}
}

func TestWindowUsesPolicyOfCurrentProvider(t *testing.T) {
	f := newContextFixture(t, "opencode-go", contextengine.Config{Routes: map[string]contextengine.RouteConfig{
		"opencode-go": {WindowThresholdTokens: new(int64(20_000)), PostRebuildRatio: new(0.3),
			L2TargetTokens: new(int64(600)), InputBudget: new(int64(10_001))},
		"openai-codex": {WindowThresholdTokens: new(int64(28_000)), PostRebuildRatio: new(0.7), L2TargetTokens: new(int64(700))},
	}})
	f.user(t, "active", "Continue work")
	var records []sessionstore.ContextRecord
	persist := windowRecorder(t, f, &records)
	assertWindow := func(provider string, threshold, target, l2, budget int64) {
		t.Helper()
		f.b.SetRuntimeProvider(provider)
		f.b.SetContextRuntime(sessionstore.RuntimeSelection{Provider: provider, Model: "synthetic-model", ContextWindow: 50_000}, true)
		if err := f.b.PrepareContext(t.Context(), persist, true); err != nil {
			t.Fatal(err)
		}
		last := records[len(records)-1]
		if w := last.Window; last.Kind != sessionstore.ContextWindowBoundary || last.Provider != provider ||
			w.ThresholdTokens != threshold || w.RebuildTargetTokens != target || w.L2TargetTokens != l2 {
			t.Fatalf("window for %s = %+v", provider, last.Window)
		}
		built, err := f.b.Build()
		if err != nil || built.Report.Context.Budget.Input != budget {
			t.Fatalf("input budget for %s did not use its policy: %+v, %v", provider, built.Report, err)
		}
	}
	assertWindow("opencode-go", 20_000, 3_000, 600, 10_001)
	assertWindow("openai-codex", 28_000, 19_600, 700, 44_880)
	events := 0
	for _, item := range f.history {
		if r, ok := item.Data.(sessionstore.HostRecord); ok && r.Context.Policy != nil {
			events++
		}
	}
	if events != 1 {
		t.Fatalf("selection change recorded %d policy events, want one initial policy", events)
	}
}

func TestWindowUsesChildRolePolicy(t *testing.T) {
	const provider = "opencode-go"
	f := newContextFixture(t, provider, contextengine.Config{Routes: map[string]contextengine.RouteConfig{
		provider: {PostRebuildRatio: new(0.6), Roles: map[string]contextengine.RouteConfig{
			"lead": {WindowThresholdTokens: new(int64(20_000)), InputBudget: new(int64(10_001)),
				L2TargetTokens: new(int64(1000)), ToolOutputExcerptChars: new(128)},
		}},
	}})
	f.b.SetContextPolicy(nil, "lead")
	f.b.SetContextRuntime(sessionstore.RuntimeSelection{Provider: provider, Model: "synthetic-model", ContextWindow: 50_000}, true)
	f.user(t, "active", "Continue work")
	var records []sessionstore.ContextRecord
	if err := f.b.PrepareContext(t.Context(), windowRecorder(t, f, &records), true); err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Kind != sessionstore.ContextWindowBoundary ||
		records[0].Policy == nil || records[0].Policy.Role != "lead" {
		t.Fatalf("role not recorded at rebuild: %+v", records)
	}
	w := records[0].Window
	if w.ThresholdTokens != 20_000 || w.RebuildTargetTokens != 6_000 || w.L2TargetTokens != 1000 || w.ToolOutputExcerptChars != 128 {
		t.Fatalf("window did not use the child role: %+v", w)
	}
	built, err := f.b.Build()
	if err != nil || built.Report.Context.Budget.Input != 10_001 {
		t.Fatalf("admission did not use the child role: %+v, %v", built.Report, err)
	}
}

func TestWindowKeepsInputsPendingBeforeLastUserMessage(t *testing.T) {
	f := newContextFixture(t, "openai-codex", contextengine.Config{
		WindowThresholdTokens: 2000, L2TargetTokens: 1000,
	})
	f.turn(t, "old", "Raw old input", strings.Repeat("worked on the old request. ", 400))
	f.user(t, "first-pending", "First queued user input")
	f.user(t, "next-pending", "Second queued user input")
	if err := f.b.PrepareContext(t.Context(), func(r sessionstore.ContextRecord) error {
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
		WindowThresholdTokens: 800, L2TargetTokens: 400, InputBudget: 600,
	})
	f.b.SetWindowPrompt(func(llm.Item) (string, bool) { return "projected window", true })
	var records []sessionstore.ContextRecord
	persist := func(r sessionstore.ContextRecord) error {
		records = append(records, r)
		return nil
	}
	if rotated, err := f.b.PrepareToolWindow(t.Context(), persist, 600, nil); err != nil || rotated || len(records) != 0 {
		t.Fatalf("at the input budget: rotated=%t records=%d error=%v", rotated, len(records), err)
	}
	if rotated, err := f.b.PrepareToolWindow(t.Context(), persist, 601, nil); err != nil || !rotated || len(records) != 1 {
		t.Fatalf("over the input budget: rotated=%t records=%d error=%v", rotated, len(records), err)
	}
	w := records[0].Window
	if w.BeforeTokens != 601 || !slices.Equal(w.Causes, []string{"admission", sessionstore.WindowCauseMidTurn}) {
		t.Fatalf("unexpected boundary: %+v", w)
	}
}

func TestWindowKeepsRunningToolGroupAcrossRawCut(t *testing.T) {
	f := newContextFixture(t, "openai-codex", contextengine.Config{
		WindowThresholdTokens: 2000, L2TargetTokens: 1000,
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
	if err := f.b.PrepareContext(t.Context(), func(r sessionstore.ContextRecord) error {
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

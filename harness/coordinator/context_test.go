package coordinator

import (
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
)

func appendOnlyTestBuilder(t *testing.T) contextbuilder.Builder {
	t.Helper()
	b := contextbuilder.NewBuilder()
	if _, err := contextbuilder.EnableWindow(b, contextengine.Config{}, contextengine.Constraints{}); err != nil {
		t.Fatal(err)
	}
	b.(interface{ SetRuntimeProvider(string) }).SetRuntimeProvider("openai-codex")
	b.SetModel(llm.Model{ID: "fixture-model"})
	return b
}

func TestContextOverflowRebuildsAndRetriesExactlyOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 0)
		store, err := localfile.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Create(t.Context(), "session-1"); err != nil {
			t.Fatal(err)
		}
		previous := session.TurnID("")
		for i := range 10 {
			id := session.TurnID(fmt.Sprintf("old-%d", i))
			if err := store.AppendInput(t.Context(), "session-1", externalEvent(t, i, "input-"+inbox.ID(id), strings.Repeat("older conversation ", 4000))); err != nil {
				t.Fatal(err)
			}
			if err := store.AppendTurn(t.Context(), "session-1", session.Turn{ID: id, PreviousTurnID: previous, Type: session.TurnRegular}); err != nil {
				t.Fatal(err)
			}
			if err := store.AppendModelResponse(t.Context(), "session-1", sessionstore.ModelResponse{TurnID: id, Response: textResponse("Earlier answer.")}); err != nil {
				t.Fatal(err)
			}
			previous = id
		}
		run.current.dependencies.ContextBuilder = appendOnlyTestBuilder(t)
		restoreTestRun(t, run, store)
		run.start(t)
		run.input(t, externalEvent(t, 20, "latest", "Continue"))
		if len(run.calls) != 1 {
			t.Fatal("missing ordinary request")
		}
		original, err := json.Marshal(run.calls[0].request)
		if err != nil || !strings.Contains(string(original), "older conversation") {
			t.Fatal("fixture did not start with the raw history", err)
		}
		overflow := llm.Response{Failure: &llm.Failure{Code: "context_length_exceeded"}}
		run.respond(t, 0, overflow)
		if len(run.calls) != 2 {
			t.Fatalf("requests after overflow = %d, want 2", len(run.calls))
		}
		retry, err := json.Marshal(run.calls[1].request)
		if err != nil || strings.Contains(string(retry), "older conversation") ||
			!strings.Contains(string(retry), "Unreal current state at the window boundary") ||
			!strings.Contains(string(retry), "Continue") {
			t.Fatal("retry did not use the rebuilt window", err)
		}
		run.respond(t, 1, overflow)
		if len(run.calls) != 2 {
			t.Fatal("retried overflow more than once")
		}
		select {
		case err := <-run.done:
			if err == nil || !strings.Contains(err.Error(), "provider context window exceeded") {
				t.Fatal("second overflow was not reported", err)
			}
		default:
			t.Fatal("second overflow left the request running")
		}
		page, err := store.Items(t.Context(), "session-1", 0, 1000)
		if err != nil {
			t.Fatal(err)
		}
		boundaries, oldInputs := 0, 0
		for _, item := range page.Items {
			if r, ok := item.Data.(sessionstore.HostRecord); ok && r.Context != nil {
				switch r.Context.Kind {
				case "summary", "summary_failed", sessionstore.ContextWindowFallback:
					t.Fatal("recorded a new summary or fallback", r.Context.Kind)
				case sessionstore.ContextWindowBoundary:
					if !slices.Contains(r.Context.Window.Causes, "provider_overflow") {
						t.Fatal("retry boundary lacks its recorded cause")
					}
					boundaries++
				}
			}
			if item.Kind == sessionstore.ItemInput {
				oldInputs++
			}
		}
		if boundaries != 1 || oldInputs != 11 {
			t.Fatalf("boundaries=%d, canonical inputs=%d", boundaries, oldInputs)
		}
		// A fresh owner reconstructs the same window from durable records.
		resumed := newStopTestRun(t, 0)
		resumed.current.dependencies.ContextBuilder = appendOnlyTestBuilder(t)
		restoreTestRun(t, resumed, store)
		if err := resumed.current.loadHistory(t.Context()); err != nil {
			t.Fatal(err)
		}
		built, err := resumed.current.dependencies.ContextBuilder.Build()
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(built.Request)
		if string(encoded) != string(retry) {
			t.Fatal("resume changed the rebuilt request")
		}
	})
}

func TestContextBudgetErrorReachesUserBeforeInference(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 0)
		b := contextbuilder.NewBuilder()
		if _, err := contextbuilder.EnableWindow(b, contextengine.Config{InputBudget: 1_000}, contextengine.Constraints{}); err != nil {
			t.Fatal(err)
		}
		run.current.dependencies.ContextBuilder = b
		run.start(t)
		run.input(t, externalEvent(t, 1, "too-large", strings.Repeat("依頼本文", 2_000)))
		if len(run.calls) != 0 {
			t.Fatalf("over-budget request made %d model calls", len(run.calls))
		}
		select {
		case err := <-run.done:
			if !contextengine.IsBudgetError(err) || !strings.Contains(err.Error(), "input budget 1000 tokens") ||
				!strings.Contains(err.Error(), "required L1 tail") || !strings.Contains(err.Error(), "L2") {
				t.Fatal("user did not receive the budget breakdown", err)
			}
		default:
			t.Fatal("over-budget request did not finish")
		}
	})
}

func TestAppendOnlyContextReadsOldSessionFixture(t *testing.T) {
	dir := t.TempDir()
	encoded, err := os.ReadFile("testdata/context-before-append-only.session.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "session-1.session.jsonl"), encoded, 0600); err != nil {
		t.Fatal(err)
	}
	store, err := localfile.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	run := newStopTestRun(t, 0)
	run.current.dependencies.ContextBuilder = appendOnlyTestBuilder(t)
	restoreTestRun(t, run, store)
	if err := run.current.loadHistory(t.Context()); err != nil {
		t.Fatal(err)
	}
	built, err := run.current.dependencies.ContextBuilder.Build()
	if err != nil {
		t.Fatal(err)
	}
	wire, _ := json.Marshal(built.Request)
	if !strings.Contains(string(wire), "old canonical task") || !strings.Contains(string(wire), "old canonical answer") {
		t.Fatal("old history was not derived in order")
	}
}

func TestInterruptedCodexCallRecordsFixedReceiptWithoutDispatch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 0)
		store, err := localfile.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Create(t.Context(), "session-1"); err != nil {
			t.Fatal(err)
		}
		if err := store.AppendInput(t.Context(), "session-1", externalEvent(t, 0, "task", "Resume task")); err != nil {
			t.Fatal(err)
		}
		if err := store.AppendTurn(t.Context(), "session-1", session.Turn{ID: "interrupted", Type: session.TurnRegular}); err != nil {
			t.Fatal(err)
		}
		response := llm.Response{Output: []llm.Item{{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "abandoned", Name: "Bash", Arguments: `{"command":"touch must-not-run"}`}}}}
		if err := store.AppendModelResponse(t.Context(), "session-1", sessionstore.ModelResponse{TurnID: "interrupted", Response: response}); err != nil {
			t.Fatal(err)
		}
		run.current.dependencies.ContextBuilder = appendOnlyTestBuilder(t)
		restoreTestRun(t, run, store)
		run.start(t)
		if len(run.operations.adds) != 0 || len(run.calls) != 1 {
			t.Fatal("abandoned call ran or corrective generation missing")
		}
		assertStopResult(t, run.calls[0].request, "abandoned", interruptedOperationResult)
		page, err := store.Items(t.Context(), "session-1", 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		recorded := false
		for _, item := range page.Items {
			if status, ok := item.Data.(sessionstore.ToolCallStatus); ok && status.CallID == "abandoned" && status.Status.Error == interruptedOperationResult {
				recorded = true
			}
		}
		if !recorded {
			t.Fatal("fixed interruption receipt was not canonical")
		}
		run.respond(t, 0, textResponse("Ready to continue safely."))
		run.input(t, stopInput(t, "stop", inbox.StopWhenIdle))
		run.assertStopped(t)
	})
}

func TestContextAdmissionMeasurementPersistsOnItsTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 0)
		store, err := localfile.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Create(t.Context(), "session-1"); err != nil {
			t.Fatal(err)
		}
		run.current.dependencies.ContextBuilder = appendOnlyTestBuilder(t)
		restoreTestRun(t, run, store)
		run.start(t)
		run.input(t, externalEvent(t, 1, "task", "Continue"))
		run.respond(t, 0, llm.Response{Usage: llm.Usage{InputTokens: 1234}, Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "answer"}}}})
		run.input(t, externalEvent(t, 2, "next", "Next"))
		page, err := store.Items(t.Context(), "session-1", 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		var admissions []*session.ContextAdmission
		for _, item := range page.Items {
			if turn, ok := item.Data.(session.Turn); ok && turn.ContextAdmission != nil {
				admissions = append(admissions, turn.ContextAdmission)
			}
		}
		if len(admissions) != 2 {
			t.Fatal("admission not stored on each model Turn", len(admissions))
		}
		first, next := admissions[0], admissions[1]
		if first.Measurement != "estimated" || first.Estimator == "" || len(first.HeaderFingerprint) != 64 || first.InputTokens != first.EstimatedInputTokens || first.RequestBytes <= first.InputTokens {
			t.Fatal("initial estimate unlabelled or mixed with bytes", first)
		}
		if next.Measurement != "measured-prefix+estimated-delta" || next.MeasuredPrefixTokens != 1234 || next.InputTokens != 1234+next.EstimatedDeltaTokens {
			t.Fatal("measured prefix/delta record lost", next)
		}
		run.respond(t, 1, textResponse("Complete"))
		run.input(t, stopInput(t, "stop", inbox.StopWhenIdle))
		run.assertStopped(t)
	})
}

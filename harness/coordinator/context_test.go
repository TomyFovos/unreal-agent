package coordinator

import (
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
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
	if _, err := contextbuilder.EnableCompaction(b, contextengine.Config{}, contextengine.Constraints{}); err != nil {
		t.Fatal(err)
	}
	b.(interface{ SetRuntimeProvider(string) }).SetRuntimeProvider("openai-codex")
	b.SetModel(llm.Model{ID: "fixture-model"})
	return b
}

func TestContextOverflowCompactsAndRetriesExactlyOnce(t *testing.T) {
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
			if err := store.AppendInput(t.Context(), "session-1", externalEvent(t, i, "input-"+inbox.ID(id), strings.Repeat("older conversation ", 1000))); err != nil {
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
		original := run.calls[0].request
		run.respond(t, 0, llm.Response{Failure: &llm.Failure{Code: "context_length_exceeded"}})
		if len(run.calls) != 2 {
			t.Fatal("missing summary request")
		}
		summary := run.calls[1].request
		before, _ := json.Marshal(original.Input)
		prefix, _ := json.Marshal(summary.Input[:len(summary.Input)-1])
		// Only changed state records may be appended before forced compaction.
		if !strings.HasPrefix(string(prefix), strings.TrimSuffix(string(before), "]")) {
			t.Fatal("summary lost conversation prefix")
		}
		run.respond(t, 1, textResponse("Task: Continue. Earlier conversation summarized."))
		if len(run.calls) != 3 {
			t.Fatal("missing compacted retry")
		}
		retry, _ := json.Marshal(run.calls[2].request)
		if !strings.Contains(string(retry), "Conversation summary") {
			t.Fatal("retry did not use compacted view")
		}
		run.respond(t, 2, llm.Response{Failure: &llm.Failure{Code: "context_length_exceeded"}})
		if len(run.calls) != 3 {
			t.Fatal("retried overflow more than once")
		}
		run.input(t, stopInput(t, "stop", inbox.StopWhenIdle))
		run.assertStopped(t)
		page, err := store.Items(t.Context(), "session-1", 0, 1000)
		if err != nil {
			t.Fatal(err)
		}
		summaries, oldInputs := 0, 0
		for _, item := range page.Items {
			if r, ok := item.Data.(sessionstore.HostRecord); ok && r.Context != nil && r.Context.Kind == "summary" {
				summaries++
			}
			if item.Kind == sessionstore.ItemInput {
				oldInputs++
			}
		}
		if summaries != 1 || oldInputs < 11 {
			t.Fatal("canonical summary or original inputs lost")
		}
		// A fresh owner reconstructs the same model view from durable records.
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
		if !strings.Contains(string(encoded), "Conversation summary") {
			t.Fatal("resume lost compacted view")
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

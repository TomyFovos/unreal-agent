//go:build linux || darwin

package coordinator

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudeagent"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/primitives"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool/observed"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

func TestClaudeUsageLimitWaitWithChild(t *testing.T) {
	for _, tc := range []struct {
		name, reset string
		until       time.Time
		retries     int
	}{
		{"weekly UTC", "3pm (UTC)", time.Date(2000, 1, 2, 15, 0, 0, 0, time.UTC), 1},
		{"unparseable reset", "an unknown time", time.Time{}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := testclaude.New(t)
			failure, err := json.Marshal(map[string]any{"type": "result", "subtype": "success", "is_error": true,
				"result": "You've hit your weekly limit · resets " + tc.reset})
			if err != nil {
				t.Fatal(err)
			}
			fake.Set(t, testclaude.Config{AgentReply: []jsontext.Value{failure}, Exit: 1})
			client, err := claudeagent.NewClient(claudeagent.Config{Binary: fake.Binary, Directory: t.TempDir(), Results: observed.NewResults()})
			if err != nil {
				t.Fatal(err)
			}
			synctest.Test(t, func(t *testing.T) {
				// Reproduce a failure after today's 15:00 UTC reset clock.
				synctest.Sleep(20*time.Hour + 23*time.Minute)
				run := newStopTestRun(t, 0)
				store := persistTestRun(t, run)
				restoreTestRun(t, run, store)
				b := appendOnlyTestBuilder(t)
				b.(interface{ SetRuntimeProvider(string) }).SetRuntimeProvider(claudeagent.ProviderID)
				b.(interface{ SetWindowPrompt(llm.WindowPrompt) }).SetWindowPrompt(claudeagent.WindowPromptLine)
				run.current.dependencies.ContextBuilder, run.current.dependencies.LLM = b, client
				type waitUpdate struct{ retryAt, publishedAt time.Time }
				updates := make(chan waitUpdate, tc.retries+1)
				run.current.dependencies.ModelWaiting = func(ctx context.Context, until time.Time) error {
					at := time.Now().UTC()
					a := llm.ActivityRecord{Kind: "usage_limit_wait", StartedAt: at, RetryAt: until}
					if err := store.AppendHostRecord(ctx, "session-1", sessionstore.HostRecord{Version: 1, Kind: sessionstore.HostActivity, Activity: &a}); err != nil {
						return err
					}
					updates <- waitUpdate{retryAt: until, publishedAt: at}
					return nil
				}
				run.current.dependencies.ModelWaitUpdated = func(until time.Time) {
					updates <- waitUpdate{retryAt: until, publishedAt: time.Now().UTC()}
				}
				nextUpdate := func() waitUpdate {
					t.Helper()
					select {
					case update := <-updates:
						return update
					case <-time.After(time.Second):
						t.Fatal("no usage wait update after the model call")
						return waitUpdate{}
					}
				}
				history := func() []sessionstore.Item {
					t.Helper()
					page, err := store.Items(t.Context(), "session-1", 0, 100)
					if err != nil {
						t.Fatal(err)
					}
					return page.Items
				}
				waitRecords := func() []llm.ActivityRecord {
					var waits []llm.ActivityRecord
					for _, item := range history() {
						if record, ok := item.Data.(sessionstore.HostRecord); ok && record.Activity != nil && record.Activity.Kind == "usage_limit_wait" {
							waits = append(waits, *record.Activity)
						}
					}
					return waits
				}
				const report = "The child completed its review."
				run.current.dependencies.CompletedInput = func(operation.Operation) (*inbox.Input, error) {
					in := progressPeerInput(t, "review-ended", "ended", report)
					return &in, nil
				}
				run.start(t)
				startModelFailureTestChild(t, run)
				run.input(t, externalEvent(t, 0, "work", "Review the patch."))
				synctest.Sleep(2 * slurpIdleTimeout)
				synctest.Wait()
				if calls, waits := fake.Calls(t), waitRecords(); len(waits) != 1 || len(calls) != 1 {
					t.Fatal("expected one model call and one usage wait", waits, len(calls))
				}
				run.assertRunning(t)
				child := run.current.state.operations["child-op"]
				if child.ID == "" || operationIsTerminal(child.Status) || len(run.operations.cancels) != 0 {
					t.Fatal("waiting affected the live child", child.Status, run.operations.cancels)
				}
				const question = "Which test should I run?"
				run.input(t, progressPeerInput(t, "question", "message", question))
				child.Status = operation.StatusCompleted
				run.operations.updates <- child
				synctest.Wait()
				synctest.Sleep(2 * slurpIdleTimeout)
				synctest.Wait()
				items := history()
				for _, id := range []inbox.ID{"question", "review-ended"} {
					if !slices.ContainsFunc(items, func(item sessionstore.Item) bool {
						in, ok := item.Data.(inbox.Input)
						return ok && in.ID == id
					}) {
						t.Fatal("child input was not saved during wait", id)
					}
				}
				if run.current.state.operations[child.ID].Status != operation.StatusCompleted || len(run.operations.cancels) != 0 {
					t.Fatal("child did not complete normally during the wait", run.operations.cancels)
				}
				backoff := primitives.DefaultRemoteRequest("", "", "").RetryPolicy.MaxBackoff
				for i := range tc.retries {
					update := nextUpdate()
					waits := waitRecords()
					if len(waits) != 1 || waits[0].RetryAt.IsZero() {
						t.Fatal("expected the same open wait for each retry", waits)
					}
					want := tc.until
					if want.IsZero() {
						want = update.publishedAt.Add(backoff)
					}
					if !update.retryAt.Equal(want) {
						t.Fatalf("waiting until %v, want %v", update.retryAt, want)
					}
					next := update.publishedAt.Add(min(5*time.Minute, want.Sub(update.publishedAt)))
					synctest.Sleep(time.Until(next) - time.Nanosecond)
					synctest.Wait()
					if calls := fake.Calls(t); len(calls) != i+1 {
						t.Fatal("model retried before its scheduled time", len(calls))
					}
					if i == tc.retries-1 {
						fake.Set(t, testclaude.Config{AgentFinal: "Resumed automatically."})
					}
					synctest.Sleep(time.Nanosecond + 4*slurpIdleTimeout)
					synctest.Wait()
				}
				if closed := nextUpdate(); !closed.retryAt.IsZero() {
					t.Fatal("usage wait was not closed", closed)
				}
				synctest.Wait()
				calls, waits := fake.Calls(t), waitRecords()
				if len(calls) != tc.retries+1 || len(waits) != 2 || !waits[1].RetryAt.IsZero() {
					t.Fatal("expected the scheduled retries and a single cleared wait", len(calls), waits)
				}
				if pending := run.current.pendingInputs(); pending != 0 {
					t.Fatal("successful retry did not acknowledge its inputs", pending)
				}
				prompt := calls[len(calls)-1].Input
				for _, text := range []string{question, report} {
					if strings.Count(prompt, text) != 1 {
						t.Fatalf("resumed prompt lost or duplicated %q: %q", text, prompt)
					}
				}
				run.input(t, stopInput(t, "stop", inbox.StopHard))
				run.assertStopped(t)
			})
		})
	}
}

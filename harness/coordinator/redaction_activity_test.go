package coordinator

import (
	"context"
	"encoding/json/v2"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
)

func TestRedactionActivityPersistsAtEventBoundaryAndIsNotRecountedOnResume(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store, err := localfile.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Create(t.Context(), "session-1"); err != nil {
			t.Fatal(err)
		}
		run := newStopTestRun(t, 0)
		run.current.dependencies.ContextBuilder = appendOnlyTestBuilder(t)
		restoreTestRun(t, run, store)
		record := func(ctx context.Context, a llm.ActivityRecord) error {
			return store.AppendHostRecord(ctx, "session-1", sessionstore.HostRecord{Version: 1, Kind: sessionstore.HostActivity, Activity: &a})
		}
		run.current.dependencies.RecordActivity = record
		run.start(t)
		run.input(t, externalEvent(t, 1, "user", "keep task token=synthetic-private"))
		run.respond(t, 0, textResponse("keep answer token=synthetic-private"))
		run.input(t, stopInput(t, "stop", inbox.StopWhenIdle))
		run.assertStopped(t)
		countRecords := func() int {
			page, err := store.Items(t.Context(), "session-1", 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, item := range page.Items {
				if h, ok := item.Data.(sessionstore.HostRecord); ok && h.Activity != nil && h.Activity.Kind == "context_redaction" {
					count++
					raw, err := json.Marshal(h.Activity)
					if err != nil || strings.Contains(string(raw), "synthetic-private") || strings.Contains(string(raw), "keep task") || h.Activity.Redaction.Reason != "partial_redaction" {
						t.Fatal("unsafe or invalid measurement", err)
					}
				}
			}
			return count
		}
		if count := countRecords(); count != 2 {
			t.Fatalf("want input and final answer measurements, got %d", count)
		}
		resumed := newStopTestRun(t, 0)
		resumed.current.dependencies.ContextBuilder = appendOnlyTestBuilder(t)
		resumed.current.dependencies.RecordActivity = record
		restoreTestRun(t, resumed, store)
		if err := resumed.current.loadHistory(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := resumed.current.recordContextRedactions(t.Context()); err != nil {
			t.Fatal(err)
		}
		if count := countRecords(); count != 2 {
			t.Fatalf("resume recounted projections: %d", count)
		}
		if err := resumed.current.handleInboxInput(t.Context(), externalEvent(t, 2, "new-user", "new task token=synthetic-private")); err != nil {
			t.Fatal(err)
		}
		if err := resumed.current.recordContextRedactions(t.Context()); err != nil {
			t.Fatal(err)
		}
		if count := countRecords(); count != 3 {
			t.Fatalf("resume did not record new projection: %d", count)
		}
	})
}

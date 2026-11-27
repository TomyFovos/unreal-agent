package viewer

import (
	"context"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func TestUsageLimitWaitProjectsFromCanonicalChildRecords(t *testing.T) {
	op := opFromSpec("start", startSpec(t, "parent", t.TempDir()))
	child, _, _ := DecodeSubagent("parent", op)
	parent := view("parent", "g", 1)
	parent.Operations = append(parent.Operations, op)
	m := New(SubagentOptions())
	must(t, m.Replace("parent", parent))
	until := epoch.Add(43 * time.Hour)
	record := func(n sessionstore.Sequence, until time.Time) host.HistoryItem {
		return host.HistoryItem{Sequence: n, RecordedAt: epoch, Kind: sessionstore.ItemHostRecord, Data: sessionstore.HostRecord{Version: 1, Kind: sessionstore.HostActivity, Activity: &llm.ActivityRecord{Kind: "usage_limit_wait", RetryAt: until}}}
	}
	wait := record(1, until)
	must(t, m.Replace(child.ID, view(child.ID, "", 0, wait)))
	if got := row(t, m, child.ID); !got.UsageWaitUntil.Equal(until) {
		t.Fatal(got)
	}
	updated := until.Add(time.Hour)
	live := view(child.ID, "", 0)
	live.History.NextAfter = wait.Sequence
	live.Progress = &host.Progress{Mode: "usage_limit_wait", RetryAt: updated}
	must(t, m.AppendPage(child.ID, wait.Sequence, live))
	if got := row(t, m, child.ID); !got.UsageWaitUntil.Equal(updated) {
		t.Fatalf("parent row did not update without new history: %v", got.UsageWaitUntil)
	}
	must(t, m.Replace(child.ID, view(child.ID, "", 0, wait, record(2, time.Time{}))))
	if got := row(t, m, child.ID); !got.UsageWaitUntil.IsZero() {
		t.Fatal("stale wait after resume", got)
	}
}

func TestUsageLimitWaitExposesLiveChildResetInTopologyAndAnalysis(t *testing.T) {
	f := ownershipFixture(t)
	r := ParentReader{Host: f.host, ParentID: f.parent.ID, Directory: f.dir, cache: newParentReadCache()}
	initial := time.Now().Add(2 * time.Hour)
	must(t, f.child.RecordActivity(t.Context(), llm.ActivityRecord{Kind: "usage_limit_wait", RetryAt: initial}))
	before, err := r.InspectTopology(t.Context(), f.child.ID, 0, 128)
	must(t, err)
	if before.History.More {
		t.Fatal("fixture has more history")
	}
	updated := initial.Add(time.Hour)
	f.parent.SetChildModelProgress(f.child.ID, host.ModelProgress{UsageRetryAt: updated})
	for _, inspect := range []func(context.Context, session.ID, sessionstore.Sequence, int) (host.View, error){r.InspectTopology, r.InspectTree} {
		after, err := inspect(t.Context(), f.child.ID, before.History.NextAfter, 128)
		must(t, err)
		if got := after.Progress.UsageRetryAt(); !got.Equal(updated) || len(after.History.Items) != 0 {
			t.Fatalf("live reset = %v with %d new history items", got, len(after.History.Items))
		}
	}
}

package viewer

import (
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
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
	must(t, m.Replace(child.ID, view(child.ID, "", 0, wait, record(2, time.Time{}))))
	if got := row(t, m, child.ID); !got.UsageWaitUntil.IsZero() {
		t.Fatal("stale wait after resume", got)
	}
}

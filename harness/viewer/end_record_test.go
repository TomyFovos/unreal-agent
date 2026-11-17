package viewer

import (
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func TestRestartEndFreezesChildAtLastRecordedTimeWithoutFinish(t *testing.T) {
	op := opFromSpec("start", startSpec(t, "parent", t.TempDir()))
	op.Status = operation.StatusCanceled
	child, _, _ := DecodeSubagent("parent", op)
	parent := view("parent", "g", 1)
	parent.Operations = []operation.Operation{op}
	m := New(SubagentOptions())
	must(t, m.Replace("parent", parent))
	end := epoch.Add(2 * time.Hour)
	item := host.HistoryItem{Sequence: 1, RecordedAt: epoch.Add(137 * time.Hour), Kind: sessionstore.ItemHostRecord, Data: sessionstore.HostRecord{Version: 1, Kind: sessionstore.HostEnded, End: &sessionstore.EndRecord{EndedBy: "host_restart", EndedAt: end}}}
	v := view(child.ID, "", 0, item)
	v.Session.Session.CreatedAt = epoch
	must(t, m.Replace(child.ID, v))
	got := row(t, m, child.ID)
	if got.End == nil || got.End.EndedBy != "host_restart" || !got.End.EndedAt.Equal(end) || got.Finish != nil || got.Runtime != RuntimeStopped || !got.Elapsed.Known || got.Elapsed.Value != 2*time.Hour {
		t.Fatal("restart time was counted as child execution", got)
	}
}

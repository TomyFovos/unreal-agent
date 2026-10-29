package viewer

import (
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/operation"
)

func TestRowsProjectGrandchildParentLivenessWithoutChangingCanonicalStatus(t *testing.T) {
	for _, running := range []bool{true, false} {
		m := New(SubagentOptions())
		v := view("lead", "lead-generation", 1)
		v.Running = running
		v.Operations = []operation.Operation{opFromSpec("start-worker", startSpec(t, "lead", t.TempDir()))}
		child, ok, err := DecodeSubagent("lead", v.Operations[0])
		must(t, err)
		if !ok {
			t.Fatal("child not decoded")
		}
		must(t, m.Replace("lead", v))
		want := RuntimeStopped
		if running {
			want = RuntimeRunning
		}
		got := row(t, m, child.ID)
		if got.ParentRuntime != want || got.ParentOperationStatus != operation.StatusAwaiting || got.Finish != nil {
			t.Fatalf("running=%v child=%+v", running, got)
		}
		// Old records have no runtime generation. They must also carry the
		// parent's unknown liveness, rather than imply that its workers run.
		v.Generation = ""
		v.Running = false
		must(t, m.Replace("lead", v))
		if got = row(t, m, child.ID); got.ParentRuntime != RuntimeUnknown || got.ParentOperationStatus != operation.StatusAwaiting {
			t.Fatalf("old lead child=%+v", got)
		}
	}
}

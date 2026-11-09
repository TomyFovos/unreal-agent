package coordinator

import (
	"reflect"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
)

func TestPeerFormatCutoverIsDurableAndLeavesRecordedInputsIntact(t *testing.T) {
	directory := t.TempDir()
	store, err := localfile.New(directory)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(t.Context(), "session-1"); err != nil {
		t.Fatal(err)
	}
	old := progressPeerInput(t, "old", "message", "old report")
	if err := store.AppendInput(t.Context(), "session-1", old); err != nil {
		t.Fatal(err)
	}
	run := newStopTestRun(t, 0)
	run.current.dependencies.ContextBuilder = appendOnlyTestBuilder(t)
	restoreTestRun(t, run, store)
	if err := run.current.loadHistory(t.Context()); err != nil {
		t.Fatal(err)
	}
	before, err := run.current.dependencies.ContextBuilder.Build()
	if err != nil || !strings.Contains(requestDump(t, before.Request), "Peer agent message") {
		t.Fatal("old report did not keep its envelope", err)
	}
	fresh := progressPeerInput(t, "new", "message", "new report")
	if err := run.current.handleInboxInputs(t.Context(), []inbox.Input{fresh}); err != nil {
		t.Fatal(err)
	}
	after, err := run.current.dependencies.ContextBuilder.Build()
	if err != nil || !reflect.DeepEqual(before.Request.Input, after.Request.Input[:len(before.Request.Input)]) {
		t.Fatal("new report changed the old prefix", err)
	}
	reopened, err := localfile.New(directory)
	if err != nil {
		t.Fatal(err)
	}
	resumed := newStopTestRun(t, 0)
	resumed.current.dependencies.ContextBuilder = appendOnlyTestBuilder(t)
	restoreTestRun(t, resumed, reopened)
	if err := resumed.current.loadHistory(t.Context()); err != nil {
		t.Fatal(err)
	}
	built, err := resumed.current.dependencies.ContextBuilder.Build()
	if err != nil || !reflect.DeepEqual(after.Request, built.Request) {
		t.Fatal("restart changed the cached prefix", err)
	}
	page, err := reopened.Items(t.Context(), "session-1", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var inputs []inbox.Input
	boundaries := 0
	for _, item := range page.Items {
		if input, ok := item.Data.(inbox.Input); ok {
			inputs = append(inputs, input)
		}
		if r, ok := item.Data.(sessionstore.HostRecord); ok && r.Context != nil && r.Context.Kind == "peer_format" {
			boundaries++
		}
	}
	if boundaries != 1 || !reflect.DeepEqual(inputs, []inbox.Input{old, fresh}) {
		t.Fatal("canonical inputs changed or cutover was not recorded once", boundaries, inputs)
	}
}

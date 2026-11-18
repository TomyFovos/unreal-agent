package localfile

import (
	"bytes"
	"encoding/json/v2"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/contextstate"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/record"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func TestContextStateFullReplayEqualsCheckpointAndTail(t *testing.T) {
	ctx := t.Context()
	store, id, _ := newRestoreFixture(t)
	seq, err := store.AppendInputRecorded(ctx, id, inbox.Input{ID: "original", Kind: inbox.InputExternal, Payload: []byte(`"exact user request"`)})
	mustCheckpoint(t, err)
	source := record.Input(string(id), uint64(seq), record.UserMessage)
	active, err := contextstate.NewEvent("request-active", contextstate.RequestChanged, contextstate.Request{
		ID: "request", Status: contextstate.RequestActive, Origin: contextstate.HumanInput, Text: "exact user request", Locator: source,
	}, source)
	mustCheckpoint(t, err)
	plan, err := contextstate.NewEvent("plan-todo", contextstate.PlanChanged, contextstate.PlanStep{ID: "task", Status: contextstate.PlanTodo, Text: "process task"})
	mustCheckpoint(t, err)
	start, err := store.AppendContextEvent(ctx, id, active)
	mustCheckpoint(t, err)
	planLocator, err := store.AppendContextEvent(ctx, id, plan)
	mustCheckpoint(t, err)
	checkpointWriteHead(t, store, id)
	checkpoint := readCheckpointFixtureFile(t, store.checkpointPath(id))
	done, err := contextstate.NewEvent("plan-done", contextstate.PlanChanged, contextstate.PlanStep{ID: "task", Status: contextstate.PlanDone})
	mustCheckpoint(t, err)
	doneLocator, err := store.AppendContextEvent(ctx, id, done)
	mustCheckpoint(t, err)
	future := contextstate.Event{Version: contextstate.Version + 1, ID: "future", Kind: contextstate.PlanChanged, Data: []byte(`{"future_data":[1,2]}`),
		Extra: []byte(`{"future_member":{"nested":true}}`)}
	futureLoc, err := store.AppendContextEvent(ctx, id, future)
	mustCheckpoint(t, err)
	if !bytes.Equal(checkpoint, readCheckpointFixtureFile(t, store.checkpointPath(id))) {
		t.Fatal("fixture did not leave a canonical tail after the checkpoint")
	}
	full, err := New(store.directory)
	mustCheckpoint(t, err)
	fullState, err := full.ContextState(ctx, id)
	mustCheckpoint(t, err)
	want, err := json.Marshal(fullState)
	mustCheckpoint(t, err)

	reopened, err := New(store.directory)
	mustCheckpoint(t, err)
	view, err := reopened.OpenCheckpointedRestoreView(ctx, id)
	mustCheckpoint(t, err)
	defer view.Close()
	if view.index.savedSize == 0 {
		t.Fatal("test fell back to full replay")
	}
	_, _, err = view.Take(ctx, id)
	mustCheckpoint(t, err)
	got, err := reopened.ContextState(ctx, id)
	mustCheckpoint(t, err)
	encoded, err := json.Marshal(got)
	mustCheckpoint(t, err)
	if !bytes.Equal(encoded, want) {
		t.Fatalf("full and checkpoint state bytes differ\nfull: %s\ncheckpoint: %s", want, encoded)
	}
	if got.Requests.Current["request"].Text != "exact user request" || got.Plan.Processed.Count != 1 || len(got.Plan.Steps) != 0 {
		t.Fatalf("prefix/suffix event lost: %s", encoded)
	}
	for _, src := range []record.Locator{planLocator, doneLocator} {
		if _, ok := got.Plan.Processed.Sources[src]; !ok {
			t.Fatal("closed plan lost its original or close record", src)
		}
	}
	// Both prefix and tail IDs remain idempotent. Unknown schema bytes are
	// canonical evidence even though they do not change the known plan slot.
	before := readCheckpointFixtureFile(t, store.sessionPath(id))
	for _, pair := range []struct {
		event contextstate.Event
		want  record.Locator
	}{{active, start}, {done, doneLocator}, {future, futureLoc}} {
		loc, err := reopened.AppendContextEvent(ctx, id, pair.event)
		if err != nil || loc != pair.want {
			t.Fatalf("retry %s = %s (%v), want %s", pair.event.ID, loc, err, pair.want)
		}
	}
	if !bytes.Equal(before, readCheckpointFixtureFile(t, store.sessionPath(id))) {
		t.Fatal("duplicate event appended another canonical record")
	}
	log, err := reopened.ReadRecordLog(ctx, id)
	mustCheckpoint(t, err)
	address, err := futureLoc.Address()
	mustCheckpoint(t, err)
	raw := log.RawItem(sessionstore.Sequence(address.Sequence))
	if !bytes.Contains(raw, future.Data) || !bytes.Contains(raw, []byte(`"future_member":{"nested":true}`)) {
		t.Fatalf("unknown schema data lost: %s", raw)
	}
}

func TestForkRetainsContextEvidenceAndStartsWithOwnedState(t *testing.T) {
	ctx := t.Context()
	store, parent, _ := newRestoreFixture(t)
	inherited, err := contextstate.NewEvent("same-id", contextstate.RuleRecorded, contextstate.Rule{ID: "parent", Excerpt: "parent rule"})
	mustCheckpoint(t, err)
	_, err = store.AppendContextEvent(ctx, parent, inherited)
	mustCheckpoint(t, err)
	mustCheckpoint(t, store.AppendTurn(ctx, parent, session.Turn{ID: "turn", Type: session.TurnRegular}))
	const child = session.ID("forked")
	_, err = store.Fork(ctx, child, parent, "turn")
	mustCheckpoint(t, err)
	lease, err := store.AcquireWriter(child)
	mustCheckpoint(t, err)
	defer lease.Close()
	empty, err := store.ContextState(ctx, child)
	mustCheckpoint(t, err)
	if len(empty.Rules) != 0 {
		t.Fatalf("fork adopted the original Session's owned state: %+v", empty.Rules)
	}
	childEvent, err := contextstate.NewEvent("same-id", contextstate.SessionChanged,
		contextstate.Session{ID: string(child), Started: time.Now().UTC()})
	mustCheckpoint(t, err)
	_, err = store.AppendContextEvent(ctx, child, childEvent)
	mustCheckpoint(t, err)
	projected, err := store.ContextState(ctx, child)
	mustCheckpoint(t, err)
	if projected.Session.ID != string(child) || len(projected.Session.Sources) != 1 {
		t.Fatal("fork suppressed its own first event")
	}
	checkpointWriteHead(t, store, child)
	assertCheckpointMatchesCanonical(t, store, child, true)
	log, err := store.ReadRecordLog(ctx, child)
	mustCheckpoint(t, err)
	inheritedIndex := slices.IndexFunc(log.Items, func(item sessionstore.Item) bool { return item.Kind == sessionstore.ItemContextEvent })
	if inheritedIndex < 0 || !reflect.DeepEqual(log.Items[inheritedIndex].Data.(contextstate.Event).Data, inherited.Data) {
		t.Fatal("fork dropped inherited context evidence")
	}
}

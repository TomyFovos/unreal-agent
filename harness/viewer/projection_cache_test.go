package viewer

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func TestRowsDecodeChildOnlyWhenOperationChanges(t *testing.T) {
	decodes := 0
	options := SubagentOptions()
	options.DecodeChild = func(id session.ID, op operation.Operation) (Child, bool, error) {
		decodes++
		return DecodeSubagent(id, op)
	}
	m := New(options)
	op := opFromSpec("spawn", startSpec(t, "parent", t.TempDir()))
	v := view("parent", "g", 1)
	v.Operations = []operation.Operation{op}
	must(t, m.Replace("parent", v))
	for i := range 20 {
		m.Rows(epoch.Add(time.Duration(i) * time.Second))
	}
	if decodes != 1 {
		t.Fatal("Rows decoded unchanged Operations", decodes)
	}
	v.Revision++
	must(t, m.AppendPage("parent", 0, v))
	if decodes != 1 {
		t.Fatal("unchanged snapshot decoded child again", decodes)
	}
	op.Status = operation.StatusCompleted
	must(t, m.Apply("parent", host.Event{Kind: "operation", Generation: "g", Revision: 3, Operation: &op}))
	rows := m.Rows(epoch)
	if decodes != 2 || rows[1].ParentOperationStatus != operation.StatusCompleted {
		t.Fatal("changed Operation was not decoded/projected", decodes, rows)
	}
	state, err := operation.DecodeRemoteJobState(op)
	must(t, err)
	state.Plan.Version = 99
	op.State, err = json.Marshal(state)
	must(t, err)
	must(t, m.Apply("parent", host.Event{Kind: "operation", Generation: "g", Revision: 4, Operation: &op}))
	if got := m.Rows(epoch); decodes != 3 || len(got) != 1 || got[0].Problem == "" {
		t.Fatal("updated invalid child plan reused old identity", decodes, got)
	}
}

type cursorReader struct {
	current               host.View
	subscribed, inspected []sessionstore.Sequence
}

func (r *cursorReader) page(after sessionstore.Sequence, limit int) host.View {
	v := r.current
	items := v.History.Items
	start := min(int(after), len(items))
	end := min(start+limit, len(items))
	next := after
	if end > start {
		next = items[end-1].Sequence
	}
	v.History = host.HistoryPage{Items: items[start:end], NextAfter: next, More: end < len(items)}
	return v
}
func (r *cursorReader) Inspect(_ context.Context, _ session.ID, after sessionstore.Sequence, limit int) (host.View, error) {
	r.inspected = append(r.inspected, after)
	return r.page(after, limit), nil
}
func (r *cursorReader) Subscribe(_ context.Context, _ session.ID, after sessionstore.Sequence, limit, _ int) (host.Subscription, error) {
	r.subscribed = append(r.subscribed, after)
	return host.Subscription{Initial: r.page(after, limit), Cancel: func() {}, Events: events()}, nil
}

func TestStoppedWatchReusesHistoryCursorAndResetsForNewGeneration(t *testing.T) {
	r := &cursorReader{current: view("s", "g", 1, response(1, "first", 10, 2))}
	r.current.Running = false
	c := NewClient(r, nil, nil)
	for range 3 {
		must(t, c.Watch(t.Context(), "s", nil))
	}
	if !reflect.DeepEqual(r.subscribed, []sessionstore.Sequence{0, 1, 1}) || row(t, c.Model, "s").Usage.Responses != 1 {
		t.Fatal("stopped observation replayed history", r.subscribed)
	}
	r.current = view("s", "new-generation", 2, response(1, "replacement", 20, 4))
	r.current.Running = false
	must(t, c.Watch(t.Context(), "s", nil))
	if !reflect.DeepEqual(r.subscribed, []sessionstore.Sequence{0, 1, 1, 1, 0}) || row(t, c.Model, "s").Usage.Input != 20 {
		t.Fatal("generation switch did not resync", r.subscribed)
	}
}

type versionCursorReader struct {
	cursorReader
	versions int
}

func (r *versionCursorReader) Version(context.Context, session.ID) (host.ObservationVersion, error) {
	r.versions++
	return host.ObservationVersion{Generation: r.current.Generation, Revision: r.current.Revision}, nil
}

func TestStoppedWatchChecksVersionWithoutRetransmittingSnapshot(t *testing.T) {
	r := &versionCursorReader{cursorReader: cursorReader{current: view("s", "g", 1, response(1, "first", 10, 2))}}
	r.current.Running = false
	c := NewClient(r, nil, nil)
	must(t, c.Watch(t.Context(), "s", nil))
	version := c.Model.Version()
	must(t, c.Watch(t.Context(), "s", nil))
	if len(r.subscribed) != 1 || r.versions != 1 || c.Model.Version() != version {
		t.Fatal("unchanged stopped snapshot was retransmitted", r.subscribed, r.versions)
	}
	r.current.Generation = "resumed-generation"
	must(t, c.Watch(t.Context(), "s", nil))
	if got := row(t, c.Model, "s"); got.Generation != "resumed-generation" || got.Usage.Responses != 1 {
		t.Fatal("version check hid a resumed Session", got)
	}
}

func TestRefreshAppendsOnlyNewHistoryAndInvalidatesChangedOperations(t *testing.T) {
	r := &cursorReader{current: view("s", "", 0, response(1, "first", 10, 2))}
	c := NewClient(r, nil, nil)
	must(t, c.Refresh(t.Context(), "s"))
	version := c.Model.Version()
	must(t, c.Refresh(t.Context(), "s"))
	if c.Model.Version() != version {
		t.Fatal("unchanged poll invalidated projection")
	}
	r.current.History.Items = append(r.current.History.Items, response(2, "next", 20, 4))
	must(t, c.Refresh(t.Context(), "s"))
	if got := row(t, c.Model, "s"); got.Usage.Input != 30 || got.Cursor != 2 {
		t.Fatal("incremental refresh lost or duplicated canonical data", got)
	}
	r.current.Operations = []operation.Operation{{ID: "op", ToolName: "Bash", Status: operation.StatusReady}}
	must(t, c.Refresh(t.Context(), "s"))
	if got := row(t, c.Model, "s"); len(got.Operations) != 1 || got.Operations[0].Status != operation.StatusReady {
		t.Fatal("operation-only refresh was ignored", got)
	}
	if !reflect.DeepEqual(r.inspected, []sessionstore.Sequence{0, 1, 1, 2}) {
		t.Fatal("refresh re-read old history", r.inspected)
	}
}

func TestParentReadCacheObservesAppendsReplacementAndKeepsOwnershipChecks(t *testing.T) {
	f := ownershipFixture(t)
	cache := newParentReadCache()
	r := ParentReader{Host: f.host, ParentID: f.parent.ID, Directory: f.dir, cache: cache}
	first, err := r.Inspect(t.Context(), f.child.ID, 0, 128)
	must(t, err)
	observed := cache.children[f.child.ID]
	if len(first.History.Items) == 0 {
		t.Fatal("empty child fixture")
	}
	first.History.Items[0].Sequence = 999
	again, err := r.Inspect(t.Context(), f.child.ID, 0, 128)
	must(t, err)
	if again.History.Items[0].Sequence != 1 || cache.children[f.child.ID].info != observed.info {
		t.Fatal("unchanged observation was reloaded or aliased")
	}
	_, err = f.child.Submit(t.Context(), f.child.Generation, inbox.Input{ID: "cache-append", Kind: inbox.InputExternal, Payload: []byte(`"continue"`)})
	must(t, err)
	updated, err := r.Inspect(t.Context(), f.child.ID, again.History.NextAfter, 128)
	must(t, err)
	if len(updated.History.Items) == 0 {
		t.Fatal("append did not invalidate child observation")
	}
	// Stop the fixture writer before replacing only this disposable test log.
	_, err = f.child.Stop(t.Context(), f.child.Generation, inbox.StopHard, "test replacement")
	must(t, err)
	must(t, f.child.Wait(t.Context()))
	path := filepath.Join(f.dir, string(f.child.ID)+".session.jsonl")
	data, err := os.ReadFile(path)
	must(t, err)
	other := path + ".replacement"
	must(t, os.WriteFile(other, data, 0600))
	must(t, os.Rename(other, path))
	_, err = r.Inspect(t.Context(), f.child.ID, 0, 128)
	must(t, err)
	if os.SameFile(observed.info, cache.children[f.child.ID].info) {
		t.Fatal("replaced inode retained old observation")
	}
	// Replacing the original configuration with a mismatched one must be
	// checked again, even though the parent identity was already cached.
	data, err = os.ReadFile(path)
	must(t, err)
	var rewritten []byte
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte{'\n'}) {
		var record map[string]any
		must(t, json.Unmarshal(line, &record))
		if item, ok := record["data"].(map[string]any)["Item"].(map[string]any); ok {
			if value, ok := item["Data"].(map[string]any); ok && value["Kind"] == "configuration" {
				value["Configuration"].(map[string]any)["ParentID"] = "unrelated"
			}
		}
		encoded, err := json.Marshal(record)
		must(t, err)
		rewritten = append(append(rewritten, encoded...), '\n')
	}
	must(t, os.WriteFile(path, rewritten, 0600))
	if _, err = r.Inspect(t.Context(), f.child.ID, 0, 128); err == nil {
		t.Fatal("cached parent authorized a mismatched child configuration")
	}
}

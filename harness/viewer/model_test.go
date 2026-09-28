package viewer

import (
	"encoding/json/jsontext"
	"errors"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"math"
	"strings"
	"testing"
	"time"
)

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func view(id session.ID, gen string, rev uint64, items ...sessionstore.Item) host.View {
	var after sessionstore.Sequence
	if len(items) > 0 {
		after = items[len(items)-1].Sequence
	}
	return host.View{Session: sessionstore.Snapshot{Session: session.Session{ID: id, CreatedAt: epoch}}, Generation: gen, Revision: rev, Running: gen != "", History: sessionstore.Page{Items: items, NextAfter: after}}
}
func response(seq sessionstore.Sequence, id string, in, out int64) sessionstore.Item {
	return sessionstore.Item{Sequence: seq, RecordedAt: epoch.Add(time.Duration(seq) * time.Second), Kind: sessionstore.ItemModelResponse, Data: sessionstore.ModelResponse{TurnID: "turn", Response: llm.Response{ID: id, Usage: llm.Usage{InputTokens: in, OutputTokens: out}}}}
}
func input(seq sessionstore.Sequence, text string) sessionstore.Item {
	return sessionstore.Item{Sequence: seq, RecordedAt: epoch.Add(time.Duration(seq) * time.Second), Kind: sessionstore.ItemInput, Data: inbox.Input{ID: inbox.ID(text), Kind: inbox.InputExternal, Payload: jsontext.Value("\"" + text + "\"")}}
}
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func row(t *testing.T, m *Model, id session.ID) Row {
	t.Helper()
	d, ok := m.Detail(id, epoch.Add(time.Minute))
	if !ok {
		t.Fatal("missing row")
	}
	return d.Row
}

// State must be valid JSON in canonical operations; the generic fixture uses
// ToolName as identity, while production uses the typed Subagent plan decoder.
func childDecoder(_ session.ID, op operation.Operation) (Child, bool, error) {
	if op.Type != "child" {
		return Child{}, false, nil
	}
	return Child{ID: session.ID(op.ToolName), Label: "child task"}, true, nil
}
func parentView(status operation.Status) host.View {
	v := view("parent", "parent-generation", 1)
	v.Operations = []operation.Operation{{ID: "op-child", Type: "child", Status: status, ToolName: "child"}}
	return v
}
func TestPagingLatestSnapshotAndUsageDedup(t *testing.T) {
	m := New(Options{RecentLimit: 2})
	first := view("one", "g", 9, response(1, "a", 100, 20))
	first.History.More = true
	first.Operations = []operation.Operation{{ID: "op", Status: operation.StatusCompleted}}
	must(t, m.Replace("one", first))
	if got := row(t, m, "one"); !got.Usage.Partial || got.Cursor != 1 {
		t.Fatalf("%+v", got)
	}
	oldOp := operation.Operation{ID: "op", Status: operation.StatusReady}
	status := sessionstore.Item{Sequence: 2, RecordedAt: epoch.Add(2 * time.Second), Kind: sessionstore.ItemToolCallStatus, Data: sessionstore.ToolCallStatus{Operations: []operation.Operation{oldOp}}}
	final := view("one", "g", 9, status, response(3, "a", 100, 20), response(4, "b", 50, 10))
	final.Operations = first.Operations
	must(t, m.AppendPage("one", 1, final))
	got := row(t, m, "one")
	if got.Usage.Partial || got.Usage.Input != 150 || got.Usage.Output != 30 || got.Usage.Responses != 2 {
		t.Fatalf("usage: %+v", got.Usage)
	}
	if got.Operations[0].Status != operation.StatusCompleted || got.Operations[0].Elapsed.Known {
		t.Fatalf("snapshot regressed/invented timestamp: %+v", got.Operations)
	}
	if err := m.Apply("one", host.Event{Generation: "g", Revision: 9, Kind: "item", Item: &status}); !errors.Is(err, ErrStale) {
		t.Fatal(err)
	}
	d, _ := m.Detail("one", epoch)
	if len(d.Recent) != 2 || d.RecentAfter != 2 {
		t.Fatalf("cache: %+v", d)
	}
	must(t, m.Replace("one", view("one", "g", 9, response(1, "a", 100, 20))))
	if row(t, m, "one").Usage.Input != 100 {
		t.Fatal("resync double counted")
	}
}
func TestMissingCursorOutOfOrderAndGeneration(t *testing.T) {
	for _, event := range []host.Event{
		{Generation: "g", Revision: 3, Kind: "operation", Operation: &operation.Operation{ID: "a"}},
		{Generation: "g", Revision: 2, Kind: "gap"},
		{Generation: "g", Revision: 2, Kind: "item", Item: ptr(input(2, "gap"))},
	} {
		m := New(Options{})
		must(t, m.Replace("s", view("s", "g", 1)))
		if err := m.Apply("s", event); !errors.Is(err, ErrResync) {
			t.Fatal(err)
		}
		if got := row(t, m, "s"); !got.NeedsResync || got.Runtime != RuntimeUnknown {
			t.Fatalf("%+v", got)
		}
		if err := m.Apply("s", host.Event{Generation: "g", Revision: 2, Kind: "stopped"}); !errors.Is(err, ErrResync) {
			t.Fatal(err)
		}
		must(t, m.Replace("s", view("s", "new", 0)))
		if err := m.Apply("s", host.Event{Generation: "g", Revision: 999, Kind: "stopped"}); !errors.Is(err, ErrStale) {
			t.Fatal(err)
		}
		if row(t, m, "s").Runtime != RuntimeRunning {
			t.Fatal("old event replaced current")
		}
	}
	m := New(Options{})
	must(t, m.Replace("s", view("s", "g", 0)))
	bad := view("s", "g", 1, input(2, "gap"))
	if err := m.AppendPage("s", 0, bad); !errors.Is(err, ErrInvalidPage) {
		t.Fatal(err)
	}
	if validPage(0, sessionstore.Page{More: true}) || validPage(0, sessionstore.Page{NextAfter: 1}) {
		t.Fatal("invalid cursor accepted")
	}
}
func ptr[T any](v T) *T { return &v }
func TestUnknownUsageTimingAndOverflow(t *testing.T) {
	m := New(Options{})
	must(t, m.Replace("s", view("s", "", 0, response(1, "unknown", 0, 0))))
	got := row(t, m, "s")
	if got.Usage.Known || got.Elapsed.Known || got.Runtime != RuntimeUnknown || FormatUsage(got.Usage) != "unknown" {
		t.Fatalf("%+v", got)
	}
	u := llm.Usage{Raw: jsontext.Value("{}")}
	var total Usage
	addUsage(&total, u)
	if !total.Known || total.Input != 0 {
		t.Fatal("explicit zero lost")
	}
	addUsage(&total, llm.Usage{InputTokens: math.MaxInt64})
	addUsage(&total, llm.Usage{InputTokens: 1, OutputTokens: 2})
	if !total.Partial || total.Input != math.MaxInt64 || total.Output != 0 {
		t.Fatal("overflow corruption")
	}
	addUsage(&total, llm.Usage{InputTokens: -1})
	if !total.Partial {
		t.Fatal("negative accepted")
	}
	m.Disconnect("s")
	if row(t, m, "s").Elapsed.Known {
		t.Fatal("inferred disconnected elapsed")
	}
}
func TestParentOperationFinishAndTransientRuntimeRemainSeparate(t *testing.T) {
	decodeFinish := func(i sessionstore.Item) (*Finish, error) {
		if in, ok := i.Data.(inbox.Input); ok && in.ID == "finish" {
			return &Finish{OperationID: "child-finish-op", Status: "success", Summary: "explicit result"}, nil
		}
		return nil, nil
	}
	m := New(Options{DecodeChild: childDecoder, DecodeFinish: decodeFinish})
	must(t, m.Replace("parent", parentView(operation.StatusCanceled)))
	rows := m.Rows(epoch)
	if len(rows) != 2 || rows[1].Depth != 1 || rows[1].Runtime != RuntimeUnknown {
		t.Fatalf("%+v", rows)
	}
	must(t, m.Select("child"))
	child := view("child", "", 0, input(1, "finish"))
	must(t, m.Replace("child", child))
	got := row(t, m, "child")
	if got.Finish == nil || got.Finish.Status != "success" || got.ParentOperationStatus != operation.StatusCanceled || got.Runtime != RuntimeUnknown {
		t.Fatalf("%+v", got)
	}
	if !got.Elapsed.Known || got.Elapsed.Value != time.Second {
		t.Fatal("canonical elapsed missing")
	}
	got.Finish.Summary = "tampered"
	if row(t, m, "child").Finish.Summary != "explicit result" {
		t.Fatal("snapshot alias")
	}
	if m.Selected() != "child" {
		t.Fatal("selection changed")
	}
	text := RenderRows(m.Rows(epoch), "child") + RenderDetail(Detail{Row: row(t, m, "child")})
	if !strings.Contains(text, "parent op: canceled") || !strings.Contains(text, "Canonical Finish: success") {
		t.Fatal(text)
	}
}
func TestCanonicalInputsAreCopiedAndMalformedRejected(t *testing.T) {
	m := New(Options{})
	initial := view("s", "g", 0, input(1, "safe"))
	must(t, m.Replace("s", initial))
	initial.History.Items[0].Data.(inbox.Input).Payload[0] = '!'
	if _, ok := m.Detail("s", epoch); !ok {
		t.Fatal("input alias")
	}
	invalid := view("bad", "g", 0, sessionstore.Item{Sequence: 1, Kind: sessionstore.ItemInput, Data: 4})
	if m.Replace("bad", invalid) == nil {
		t.Fatal("invalid data accepted")
	}
	if strings.ContainsAny(SafeText("\x1b[31mhi\r\n\x00"), "\x1b\r\n\x00") {
		t.Fatal("terminal controls remain")
	}
}

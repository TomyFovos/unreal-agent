package viewer

import (
	"reflect"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func TestRowActivityUsesCanonicalTimesAndTenMinuteWindow(t *testing.T) {
	now := epoch.Add(30 * time.Minute)
	old := input(1, "old")
	old.RecordedAt = now.Add(-10*time.Minute - time.Nanosecond)
	boundary := input(2, "boundary")
	boundary.RecordedAt = now.Add(-10 * time.Minute)
	response := response(3, "response", 1, 1)
	response.RecordedAt = now.Add(-time.Minute)
	status := host.HistoryItem{Sequence: 4, RecordedAt: now, Kind: sessionstore.ItemToolCallStatus, Data: sessionstore.ToolCallStatus{}}
	m := New(Options{RecentLimit: 1})
	must(t, m.Replace("s", view("s", "g", 1, old, boundary, response, status)))
	d, ok := m.Detail("s", now)
	want := []time.Time{boundary.RecordedAt, response.RecordedAt, status.RecordedAt}
	if !ok || !d.Row.CreatedAt.Equal(epoch) || !d.Row.LastActivityAt.Equal(now) || !reflect.DeepEqual(d.Row.RecentActivity, want) {
		t.Fatalf("activity = %+v, want created=%v last=%v recent=%v", d.Row, epoch, now, want)
	}
	if len(d.Recent) != 1 {
		t.Fatalf("activity changed transcript cache: %d", len(d.Recent))
	}
	rows := m.Rows(now.Add(11 * time.Minute))
	if len(rows[0].RecentActivity) != 0 || !rows[0].LastActivityAt.Equal(now) || !rows[0].CreatedAt.Equal(epoch) {
		t.Fatalf("window did not expire independently of last activity: %+v", rows[0])
	}
	// Reading at another time must not prune canonical timestamps from the ring.
	if got := m.Rows(now)[0].RecentActivity; !reflect.DeepEqual(got, want) {
		t.Fatalf("read mutated activity history: %v", got)
	}
}

func TestRowActivityRingRetainsLatest256AcrossPagingAndEvents(t *testing.T) {
	m := New(Options{RecentLimit: 1})
	var items []host.HistoryItem
	for i := 1; i <= 600; i++ {
		items = append(items, input(sessionstore.Sequence(i), "activity"))
	}
	first := view("s", "g", 1, items[:200]...)
	first.History.More = true
	must(t, m.Replace("s", first))
	page := view("s", "g", 1, items[200:500]...)
	must(t, m.AppendPage("s", 200, page))
	for i := 500; i < 600; i++ {
		must(t, m.Apply("s", host.Event{Kind: "item", Generation: "g", Revision: uint64(i - 498), Item: &items[i]}))
	}
	now := epoch.Add(600 * time.Second)
	d, ok := m.Detail("s", now)
	if !ok || !d.Row.LastActivityAt.Equal(now) || !d.Row.CreatedAt.Equal(epoch) || len(d.Row.RecentActivity) != 256 {
		t.Fatalf("ring row = %+v", d.Row)
	}
	for i, at := range d.Row.RecentActivity {
		if want := epoch.Add(time.Duration(i+345) * time.Second); !at.Equal(want) {
			t.Fatalf("ring timestamp %d = %v, want %v", i, at, want)
		}
	}
	if len(d.Recent) != 1 || d.Recent[0].Sequence != 600 {
		t.Fatalf("transcript cache = %+v", d.Recent)
	}
	// Returned rows and details must not expose mutable projection memory.
	d.Row.RecentActivity[0] = time.Time{}
	rows := m.Rows(now)
	rows[0].RecentActivity[1] = time.Time{}
	if got := m.Rows(now)[0].RecentActivity; !got[0].Equal(epoch.Add(345*time.Second)) || !got[1].Equal(epoch.Add(346*time.Second)) {
		t.Fatalf("snapshot alias: %v", got[:2])
	}
}

func TestRowActivityIgnoresTransientEventsAndResetsOnReplace(t *testing.T) {
	m := New(Options{})
	item := input(1, "first")
	must(t, m.Replace("s", view("s", "g", 1, item)))
	for i, event := range []host.Event{
		{Kind: "progress"},
		{Kind: "operation", Operation: &operation.Operation{ID: "op", Status: operation.StatusReady}},
		{Kind: "stopped"},
	} {
		event.Generation, event.Revision = "g", uint64(i+2)
		must(t, m.Apply("s", event))
	}
	m.Disconnect("s")
	r := row(t, m, "s")
	if !reflect.DeepEqual(r.RecentActivity, []time.Time{item.RecordedAt}) || !r.LastActivityAt.Equal(item.RecordedAt) {
		t.Fatalf("transient events invented activity: %+v", r)
	}
	m.Invalidate("s")
	if got := row(t, m, "s"); !got.LastActivityAt.Equal(item.RecordedAt) || len(got.RecentActivity) != 1 {
		t.Fatalf("invalidation lost canonical activity: %+v", got)
	}
	newView := view("s", "new-generation", 1)
	newView.Session.Session.CreatedAt = epoch.Add(2 * time.Minute)
	must(t, m.Replace("s", newView))
	if got := row(t, m, "s"); !got.LastActivityAt.IsZero() || len(got.RecentActivity) != 0 || !got.CreatedAt.Equal(newView.Session.Session.CreatedAt) {
		t.Fatalf("replace retained old projection values: %+v", got)
	}
	must(t, m.Replace("s", view("s", "g", 1, item)))
	if got := row(t, m, "s"); !reflect.DeepEqual(got.RecentActivity, []time.Time{item.RecordedAt}) || !got.LastActivityAt.Equal(item.RecordedAt) {
		t.Fatalf("replay changed canonical activity: %+v", got)
	}
}

func TestRowActivityPreservesDuplicatesAndLatestTime(t *testing.T) {
	now := epoch.Add(5 * time.Minute)
	items := []host.HistoryItem{input(1, "a"), input(2, "b"), input(3, "c"), input(4, "d")}
	items[0].RecordedAt = now
	items[1].RecordedAt = now
	items[2].RecordedAt = now.Add(-time.Second)
	items[3].RecordedAt = time.Time{}
	m := New(Options{})
	must(t, m.Replace("s", view("s", "g", 1, items...)))
	r := m.Rows(now)[0]
	want := []time.Time{now, now, now.Add(-time.Second)}
	if !reflect.DeepEqual(r.RecentActivity, want) || !r.LastActivityAt.Equal(now) {
		t.Fatalf("timestamps were deduplicated, reordered or invented: %+v", r)
	}
	if got := m.Rows(now.Add(-time.Second))[0].RecentActivity; !reflect.DeepEqual(got, []time.Time{now.Add(-time.Second)}) {
		t.Fatalf("activity window included future timestamps: %v", got)
	}
}

func TestUnobservedChildHasNoActivityOrCreationTime(t *testing.T) {
	m := New(Options{DecodeChild: childDecoder})
	must(t, m.Replace("parent", parentView(operation.StatusAwaiting)))
	r := row(t, m, "child")
	if !r.CreatedAt.IsZero() || !r.LastActivityAt.IsZero() || len(r.RecentActivity) != 0 {
		t.Fatalf("invented unobserved child activity: %+v", r)
	}
}

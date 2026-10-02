package host

import (
	"encoding/json/v2"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/projectinstructions"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func TestProjectInstructionsPublicProjection(t *testing.T) {
	const marker = "unique-AGENTS-secret-marker-27-projection"
	workspace := t.TempDir()
	writeAgents(t, workspace, marker)
	h := newTestHost(t, t.TempDir())
	s, err := h.Create(t.Context(), Options{ID: "projection", Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	want, err := projectinstructions.FromContent([]byte(marker))
	if err != nil {
		t.Fatal(err)
	}
	assertSafe := func(value any) {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), marker) {
			t.Fatal("public result exposed AGENTS.md content")
		}
	}
	view, err := s.Inspect(0, 1)
	if err != nil {
		t.Fatal(err)
	}
	assertSafe(view)
	assertSafe(view.History)
	assertSafe(view.History.Items[0])
	publicBytes, err := json.Marshal(view.History.Items[0])
	if err != nil {
		t.Fatal(err)
	}
	var canonicalDecode sessionstore.Item
	if err := json.Unmarshal(publicBytes, &canonicalDecode); err == nil {
		t.Fatal("public metadata accepted as a canonical snapshot")
	}
	var publicDecode HistoryItem
	if err := json.Unmarshal(publicBytes, &publicDecode); err != nil {
		t.Fatal(err)
	}
	record, ok := publicDecode.Data.(ProjectInstructionRecord)
	if !ok || record.ProjectInstructions == nil || *record.ProjectInstructions != want.Metadata() {
		t.Fatal("public metadata did not round-trip")
	}
	if view.ProjectInstructions == nil || *view.ProjectInstructions != want.Metadata() {
		t.Fatalf("metadata lost: %+v", view.ProjectInstructions)
	}
	if len(view.History.Items) != 1 || view.History.Items[0].Sequence != 1 || view.History.NextAfter != 1 || !view.History.More {
		t.Fatalf("history cursor changed: %+v", view.History)
	}
	sub, err := s.Subscribe(0, 128, 16)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Cancel()
	assertSafe(sub.Initial)
	// Creation bindings precede subscription in a running Host. Exercise the
	// same observer/event path on an isolated session without inserting a second
	// binding into a live coordinator's canonical history.
	s.mu.Lock()
	canonical := s.items[0]
	s.mu.Unlock()
	observer := &Session{Generation: "event-generation", running: true,
		submissions: map[inbox.ID]*submission{}, operations: map[operation.ID]operation.Operation{},
		subscribers: map[uint64]chan Event{}}
	observer.rememberItem(canonical, false)
	events, err := observer.Subscribe(0, 128, 16)
	if err != nil {
		t.Fatal(err)
	}
	defer events.Cancel()
	assertSafe(events.Initial)
	canonical.Sequence = 2
	observer.rememberItem(canonical, true)
	event := <-events.Events
	assertSafe(event)
	if event.Item == nil || event.Item.Sequence != canonical.Sequence || event.Item.RecordedAt != canonical.RecordedAt || event.Item.Kind != canonical.Kind {
		t.Fatalf("event identity changed: %+v", event)
	}
	metadata, ok := event.Item.Data.(ProjectInstructionRecord)
	if !ok || metadata.ProjectInstructions == nil || *metadata.ProjectInstructions != want.Metadata() {
		t.Fatalf("event metadata lost: %+v", event.Item.Data)
	}
	s.mu.Lock()
	stored, err := s.store.Items(t.Context(), s.ID, 0, 128)
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	snapshot := stored.Items[0].Data.(sessionstore.HostRecord).ProjectInstructions
	if *snapshot != want {
		t.Fatal("public projection modified canonical snapshot")
	}
	stop(t, s)
}

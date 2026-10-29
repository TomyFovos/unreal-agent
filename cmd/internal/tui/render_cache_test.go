package tui

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
)

func cachedFrameOracle(t *testing.T, s Snapshot, u UIState, width, height int) Frame {
	t.Helper()
	got := RenderFrame(s, u, width, height)
	u.cache, u.index = nil, nil
	want := RenderFrame(s, u, width, height)
	if !reflect.DeepEqual(got, want) {
		for i := range min(len(got.Lines), len(want.Lines)) {
			if got.Lines[i] != want.Lines[i] {
				t.Errorf("row %d: cached %q; fresh %q", i, got.Lines[i], want.Lines[i])
			}
		}
		t.Fatalf("cached frame differs from fresh rendering (cursor %d,%d / %d,%d)", got.CursorX, got.CursorY, want.CursorX, want.CursorY)
	}
	return got
}

func TestRenderCacheInvalidationMatchesFreshFrames(t *testing.T) {
	s, u := orchestrationFixture()
	s.Version, u.Viewer.Version = 1, 1
	s.Progress = nil
	s.Entries = []Entry{{Role: "you", Text: "inspect"}, {Role: "agent", Text: "original answer", Calls: []Receipt{{Name: "Bash", Target: "go test ./...", Operations: []operation.ID{"test"}, Status: operation.StatusReady, StartedAt: u.Now.Add(-time.Second)}}}}
	s.Operations = []operation.Operation{{ID: "test", ToolName: "Bash", Status: operation.StatusReady}}
	u.cache = &bodyCache{}
	u.View = ViewSplit
	u.Viewer.Rows[1].Runtime = viewer.RuntimeRunning
	u.Viewer.Rows[1].Elapsed = viewer.Duration{Known: true, Value: time.Second}
	width, height := 160, 48
	cachedFrameOracle(t, s, u, width, height)
	steps := []struct {
		name   string
		change func()
	}{
		{"typing", func() { u.Input = "hello 日本語"; u.Cursor = len(u.Input) }},
		{"cursor", func() { u.Cursor = 1 }},
		{"entry update", func() { s.Entries[1].Text = "updated **answer**"; s.Version++ }},
		{"operation update", func() { s.Operations[0].Status = operation.StatusCompleted; s.Version++ }},
		{"child finish", func() {
			u.Viewer.Rows[1].Finish = &viewer.Finish{Status: "completed", RecordedAt: u.Now}
			u.Viewer.Version++
		}},
		{"width", func() { width = 100 }},
		{"theme", func() { u.Theme.NoColor = true }},
		{"ascii", func() { u.Theme.ASCII = true }},
		{"time and spinner", func() { s.WaitingForModel = true; s.LatestAt = u.Now; s.Version++; u.Now = u.Now.Add(3 * time.Second) }},
		{"spinner without data update", func() { u.Now = u.Now.Add(120 * time.Millisecond) }},
		{"elapsed without data update", func() { u.Now = u.Now.Add(2 * time.Second) }},
		{"approval enters", func() {
			s.Permissions = permission.ModeSnapshot{Enabled: true, Mode: permission.Ask, Revision: 1, Pending: []permission.Pending{{ID: "approval", Tool: "Bash", Scope: "go test ./..."}}}
			s.Version++
		}},
		{"approval picker", func() { u.Picker = operationPermissionConfirmation(s, s.Permissions.Pending[0]) }},
		{"approval leaves", func() { s.Permissions.Pending = nil; s.Permissions.Revision++; s.Version++; u.Picker = nil }},
		{"progress", func() { s.Progress = &host.Progress{Mode: "streaming", Text: "new draft"}; s.Version++ }},
		{"disconnect", func() { s.Connected = false; s.OfflineSince = u.Now; s.Version++ }},
		{"reconnect", func() { s.Connected = true; s.Version++ }},
		{"command menu", func() { u.Input = "/view"; u.Cursor = len(u.Input) }},
		{"plain input", func() { u.Input = "normal"; u.Cursor = len(u.Input) }},
		{"chat dock", func() { u.View = ViewChat }},
		{"chat dock repeated", func() { u.Now = u.Now.Add(time.Second) }},
		{"split", func() { u.View = ViewSplit; width = 160 }},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			step.change()
			cachedFrameOracle(t, s, u, width, height)
			u.Input += "x"
			u.Cursor = len(u.Input)
			cachedFrameOracle(t, s, u, width, height)
		})
	}
}

func TestRenderCacheRetainsBodiesAndProtectsUpdatedLabels(t *testing.T) {
	s, u := orchestrationFixture()
	s.Version, u.Viewer.Version = 1, 1
	u.cache = &bodyCache{}
	u.View = ViewSplit
	s.Progress = nil
	s.Entries = []Entry{{Role: "agent", Text: "retained markdown"}}
	cachedFrameOracle(t, s, u, 160, 48)
	if len(u.cache.entries) != 1 {
		t.Fatal("canonical body was not cached")
	}
	u.Now = u.Now.Add(time.Second)
	cachedFrameOracle(t, s, u, 160, 48)
	if len(u.cache.entries) != 1 {
		t.Fatal("clock tick discarded unchanged body formatting")
	}
	u.Viewer.Rows[1].Label = "api_key=synthetic-sensitive-value"
	u.Viewer.Version++
	f := cachedFrameOracle(t, s, u, 160, 48)
	text := strings.Join(f.Lines, "\n")
	if strings.Contains(text, "synthetic-sensitive-value") || !strings.Contains(text, "[protected]") {
		t.Fatal("updated label bypassed the secret guard")
	}
}

func TestWorkCardCacheClockUsesActiveRowsAndKeepsBeads(t *testing.T) {
	s, u := largeRenderSession()
	cachedFrameOracle(t, s, u, 160, 48)
	index := u.cache.work
	if len(index.rows) != 61 || len(index.beads) != 800 {
		t.Fatal("clock index includes completed children", len(index.rows), len(index.beads))
	}
	for _, delta := range []time.Duration{time.Second, 6 * time.Minute, 11 * time.Minute} {
		u.Now = u.Now.Add(delta)
		cachedFrameOracle(t, s, u, 160, 48)
		if u.cache.work != index {
			t.Fatal("clock rebuilt static child ordering/beads")
		}
	}
}

func TestProjectionVersionIncludesLocalPermissionAndConnectionUpdates(t *testing.T) {
	m := NewModel("s")
	initial := m.Version()
	m.status("connected", true)
	if m.Version() <= initial {
		t.Fatal("connection change did not invalidate frames")
	}
	m.mu.Lock()
	m.state.Generation = "g"
	m.mu.Unlock()
	before := m.Version()
	m.setPermissions("g", permission.ModeSnapshot{Revision: 1, Pending: []permission.Pending{{ID: "p"}}})
	if m.Snapshot().Version <= before {
		t.Fatal("permission change did not invalidate frames")
	}
}

type stoppedVersionReader struct {
	*fakeClient
	checks int
	cancel context.CancelFunc
}

func (r *stoppedVersionReader) Version(context.Context, session.ID) (host.ObservationVersion, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.checks++
	if r.checks == 2 {
		r.view.Generation = "resumed"
		r.view.Revision = 2
		r.view.History.Items = append(r.view.History.Items, host.HistoryItem{Sequence: 2, Kind: sessionstore.ItemInput, Data: inbox.Input{ID: "resumed-input", Kind: inbox.InputExternal, Payload: []byte(`"resumed message"`)}})
		r.view.History.NextAfter = 2
	}
	if r.checks == 3 {
		r.cancel()
	}
	return host.ObservationVersion{Generation: r.view.Generation, Revision: r.view.Revision}, nil
}

func TestStoppedTUIWatchUsesVersionAndObservesResume(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f := newFake()
	f.view.Running = false
	f.view.History = host.HistoryPage{Items: []host.HistoryItem{{Sequence: 1, Kind: sessionstore.ItemInput, Data: inbox.Input{ID: "initial", Kind: inbox.InputExternal, Payload: []byte(`"initial message"`)}}}, NextAfter: 1}
	close(f.events)
	r := &stoppedVersionReader{fakeClient: f, cancel: cancel}
	m := NewModel("s")
	if err := Watch(ctx, r, m, func() {}); err != context.Canceled {
		t.Fatal(err)
	}
	s := m.Snapshot()
	if r.subscribes != 2 || s.Generation != "resumed" || s.After != 2 || len(s.Entries) != 2 {
		t.Fatal("unchanged poll copied snapshot or resume was missed", r.subscribes, s.Generation, s.After)
	}
}

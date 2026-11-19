package tui

import (
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
)

type countedScreen struct {
	screenObserver
	writes atomic.Int64
}

func (s *countedScreen) Write(p []byte) (int, error) {
	s.writes.Add(1)
	return s.screenObserver.Write(p)
}

func TestIdleTUIWaitsForUpdatesAndStopsAnimation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFake()
		keys := make(chan Key, 1)
		resized := make(chan os.Signal, 1)
		updates := make(chan struct{}, 1)
		out := &countedScreen{}
		var snapshots atomic.Int64
		var panel atomic.Pointer[viewer.PanelSnapshot]
		panel.Store(&viewer.PanelSnapshot{ParentID: "s", Selected: "s", Version: 1})
		done := make(chan error, 1)
		go func() {
			done <- Run(t.Context(), Config{
				Client: f, ID: "s", Keys: keys, Output: out, Theme: &Theme{Plain: true},
				Resize: resized, Size: func() (int, int) { return 150, 32 }, Updates: updates,
				Viewer: func(time.Time) viewer.PanelSnapshot {
					snapshots.Add(1)
					return *panel.Load()
				},
			})
		}()
		defer func() {
			keys <- Key{Name: "detach"}
			if err := <-done; err != nil {
				t.Error(err)
			}
		}()
		synctest.Wait()
		if !strings.Contains(out.text(), "入力待ち") {
			t.Fatal("TUI did not reach idle")
		}
		assertQuiet := func() {
			t.Helper()
			writes, reads := out.writes.Load(), snapshots.Load()
			time.Sleep(5 * time.Second)
			synctest.Wait()
			if gotWrites, gotReads := out.writes.Load(), snapshots.Load(); gotWrites != writes || gotReads != reads {
				t.Fatalf("idle TUI repainted or rebuilt the viewer: writes %d → %d, snapshots %d → %d", writes, gotWrites, reads, gotReads)
			}
		}
		assertQuiet()

		at := time.Now()
		f.events <- host.Event{Kind: "progress", Generation: "g", Revision: 1,
			Progress: &host.Progress{Epoch: 1, Mode: "streaming", Text: "live lead text"}}
		synctest.Wait()
		if !strings.Contains(out.text(), "live lead text") || time.Now() != at {
			t.Fatal("lead update did not render immediately")
		}
		writes := out.writes.Load()
		time.Sleep(480 * time.Millisecond)
		synctest.Wait()
		if out.writes.Load() <= writes {
			t.Fatal("working lead stopped animating")
		}
		f.events <- host.Event{Kind: "progress", Generation: "g", Revision: 2}
		synctest.Wait()
		if !strings.Contains(out.text(), "入力待ち") {
			t.Fatal("lead did not return to idle")
		}
		assertQuiet()

		at = time.Now()
		panel.Store(&viewer.PanelSnapshot{ParentID: "s", Selected: "s", Version: 2,
			Rows: []viewer.Row{{ID: "worker", ParentID: "s", Label: "live worker text", ParentOperationStatus: operation.StatusReady}}})
		updates <- struct{}{}
		synctest.Wait()
		if !strings.Contains(out.text(), "live worker text") || time.Now() != at {
			t.Fatal("worker update did not render immediately")
		}
	})
}

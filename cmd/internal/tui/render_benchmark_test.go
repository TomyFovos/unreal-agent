package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
)

// Synthetic display data only; no user history or credentials are fixtures.
func largeRenderSession() (Snapshot, UIState) {
	now := time.Unix(1800000000, 0)
	s := Snapshot{Version: 1, ID: "large-main", Generation: "g", Revision: 1, Connected: true, Running: true, Status: "connected"}
	u := UIState{Theme: Theme{Depth: ColorTrue}, Now: now, View: ViewSplit, cache: &bodyCache{}}
	u.Viewer = viewer.PanelSnapshot{Version: 1, ParentID: s.ID, Selected: s.ID}
	u.Viewer.Rows = append(u.Viewer.Rows, viewer.Row{ID: s.ID, Generation: "g", Runtime: viewer.RuntimeRunning, CreatedAt: now.Add(-time.Hour)})
	for i := range 4000 {
		role := "agent"
		if i%3 == 0 {
			role = "you"
		}
		s.Entries = append(s.Entries, Entry{Role: role, Text: fmt.Sprintf("Message %d: **rendering** 日本語 and `code` with https://example.com/docs", i), At: now.Add(time.Duration(i-4000) * time.Second), Sequence: sessionstore.Sequence(i + 1), Calls: []Receipt{{CallID: fmt.Sprint(i), Name: "Read", Target: "cmd/internal/tui/render.go", Status: operation.StatusCompleted}}})
	}
	for i := range 860 {
		id := session.ID(fmt.Sprintf("child-%04d", i))
		r := viewer.Row{ID: id, ParentID: s.ID, ParentOperationID: operation.ID("spawn-" + string(id)), ParentOperationStatus: operation.StatusReady, Label: fmt.Sprintf("Task %d: inspect renderer", i), Generation: "child-g", Runtime: viewer.RuntimeRunning, Depth: 1, CreatedAt: now.Add(-time.Duration(i+1) * time.Minute), LastActivityAt: now.Add(-time.Second)}
		if i >= 60 {
			r.ParentOperationStatus, r.Runtime = operation.StatusCompleted, viewer.RuntimeStopped
			r.Finish = &viewer.Finish{Status: "completed", RecordedAt: now.Add(-time.Duration(i) * time.Second)}
		}
		for j := range 32 {
			status := operation.StatusCompleted
			if i < 60 && j == 31 {
				status = operation.StatusReady
			}
			r.Operations = append(r.Operations, viewer.OperationRow{ID: operation.ID(fmt.Sprintf("op-%04d-%02d", i, j)), Tool: "Bash", Target: "go test ./cmd/internal/tui", Status: status, Sequence: sessionstore.Sequence(j + 1)})
		}
		u.Viewer.Rows = append(u.Viewer.Rows, r)
		s.Operations = append(s.Operations, operation.Operation{ID: r.ParentOperationID, ToolName: "SubagentStart", Status: r.ParentOperationStatus})
	}
	return s, u
}

var benchmarkFrame Frame

func BenchmarkRenderFrameLargeSession(b *testing.B) {
	s, u := largeRenderSession()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		benchmarkFrame = RenderFrame(s, u, 160, 48)
	}
}

func BenchmarkRenderFrameLargeSessionKeyInput(b *testing.B) {
	s, u := largeRenderSession()
	benchmarkFrame = RenderFrame(s, u, 160, 48)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		u.Input = strings.Repeat("x", 1+i%100)
		u.Cursor = len(u.Input)
		benchmarkFrame = RenderFrame(s, u, 160, 48)
	}
}

func BenchmarkRenderFrameLargeSessionKeyInputWithClock(b *testing.B) {
	s, u := largeRenderSession()
	benchmarkFrame = RenderFrame(s, u, 160, 48)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		u.Now = u.Now.Add(8 * time.Millisecond)
		u.Input = strings.Repeat("x", 1+i%100)
		u.Cursor = len(u.Input)
		benchmarkFrame = RenderFrame(s, u, 160, 48)
	}
}

func BenchmarkRenderFrameLargeSessionClock(b *testing.B) {
	s, u := largeRenderSession()
	benchmarkFrame = RenderFrame(s, u, 160, 48)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		u.Now = u.Now.Add(time.Second)
		benchmarkFrame = RenderFrame(s, u, 160, 48)
	}
}

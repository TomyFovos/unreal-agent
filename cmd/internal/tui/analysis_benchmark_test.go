//go:build linux || darwin

package tui

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/host/gateway"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
	"github.com/unreallabsai/unreal-agent/internal/testsession"
)

// BenchmarkAnalysisHistory measures opening analysis from a lazy transcript over
// a Unix socket, through the report used for the first Overview. Setup and
// Host resume are outside the measurement.
func BenchmarkAnalysisHistory(b *testing.B) {
	dir := b.TempDir()
	for name, data := range testsession.StartupLogs(b, dir, map[string]any{}) {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			b.Fatal(err)
		}
	}
	h, err := host.New(b.Context(), host.Config{Directory: dir, Build: func(ctx context.Context, _ session.ID) (host.Runtime, error) {
		return host.Runtime{Builder: contextbuilder.NewBuilder(), LLM: startupAdapter{}, Tools: tool.NewRegistry(tool.StaticTranslators{}), Operations: operation.NewLocalOperationManager(ctx)}, nil
	}})
	if err != nil {
		b.Fatal(err)
	}
	defer h.Close()
	owner, err := h.Resume(b.Context(), host.Options{ID: "main"})
	if err != nil {
		b.Fatal(err)
	}
	socketDir, err := os.MkdirTemp("", "ua-analysis-")
	if err != nil {
		b.Fatal(err)
	}
	defer os.RemoveAll(socketDir)
	socket := filepath.Join(socketDir, "host.sock")
	server, err := gateway.StartSessionCommands(b.Context(), socket, gateway.Config{Host: h, Extension: viewer.NewExtensionHandler(h, dir)})
	if err != nil {
		b.Fatal(err)
	}
	defer server.Close()
	client := gateway.NewClient(socket)
	defer client.Close()
	remote := viewer.Remote{ParentID: owner.ID, Parent: client, Call: client.Extension}
	panel := viewer.NewPanel(b.Context(), owner.ID, viewer.NewClient(remote, viewer.New(viewer.SubagentOptions()), remote))
	defer panel.Close()
	tail, err := owner.InspectBefore(0, historyPageItems)
	if err != nil || !tail.History.Older {
		b.Fatal("expected a lazy transcript", err)
	}
	m := NewModel(owner.ID)
	if err := m.seedTail(tail); err != nil {
		b.Fatal(err)
	}
	var cold, warm time.Duration
	for b.Loop() {
		start := time.Now()
		now := time.Now()
		v := panel.Snapshot(now)
		report, err := m.readAnalysisTree(b.Context(), remote.InspectTree, now, v.Rows, v.SubscriptionUsage)
		if err != nil {
			b.Fatal(err)
		}
		if report.Tree.Partial || len(report.Tree.Sessions) != 201 || len(report.Turns) != 2001 || report.AgentCounts.Observed != 200 {
			b.Fatal("incomplete analysis", report.Tree.Partial, len(report.Tree.Sessions), len(report.Turns), report.AgentCounts)
		}
		if cold == 0 {
			cold = time.Since(start)
		} else {
			warm += time.Since(start)
		}
	}
	b.ReportMetric(float64(cold.Microseconds())/1000, "ms/first")
	if b.N > 1 {
		b.ReportMetric(float64(warm.Microseconds())/1000/float64(b.N-1), "ms/reopen")
	}
}

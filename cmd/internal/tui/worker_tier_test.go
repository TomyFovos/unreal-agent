package tui

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/analysis"
	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
	"github.com/unreallabsai/unreal-agent/internal/testsession"
)

func TestWorkerTierTUIReadsArchivedGrandchildrenWithoutResuming(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sessions")
	f := testsession.WriteWorkerTierTree(t, dir)
	store, err := localfile.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if moved, err := store.ArchiveChild(t.Context(), localfile.ArchiveEntry{ChildID: f.Workers["qwen-archived"], ParentID: f.Lead, OperationID: "qwen-archived", Status: operation.StatusCompleted}); err != nil || !moved {
		t.Fatal(moved, err)
	}
	h, err := host.New(t.Context(), host.Config{Directory: dir, Build: func(ctx context.Context, _ session.ID) (host.Runtime, error) {
		return host.Runtime{Builder: contextbuilder.NewBuilder(), LLM: archiveAnalysisModel{}, Tools: tool.NewRegistry(tool.StaticTranslators{}), Operations: operation.NewLocalOperationManager(ctx)}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	_, items, _, err := store.ReadSnapshot(t.Context(), "main")
	if err != nil {
		t.Fatal(err)
	}
	owner, err := h.Resume(t.Context(), host.Options{ID: "main", Configuration: items[0].Data.(sessionstore.HostRecord).Configuration})
	if err != nil {
		t.Fatal(err)
	}
	r := viewer.ParentReader{Host: h, ParentID: "main", Directory: dir}
	m := NewModel("main")
	v, err := owner.Inspect(0, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.apply(v); err != nil {
		t.Fatal(err)
	}
	handler := viewer.NewExtensionHandler(h, dir)
	remote := viewer.Remote{ParentID: "main", Parent: r, Call: func(ctx context.Context, raw jsontext.Value) (jsontext.Value, error) {
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, "http://localhost/v1/extension", bytes.NewReader(raw))
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, req)
		return jsontext.Value(out.Body.Bytes()), nil
	}}
	if err := m.LoadAnalysisTree(t.Context(), remote.InspectTree); err != nil {
		t.Fatal(err)
	}
	report := m.Analysis(time.Now(), nil)
	if report.Tree == nil || report.Tree.Partial || len(report.Tree.Sessions) != 8 || len(report.Tree.Workers) != 2 {
		t.Fatal("TUI omitted grandchildren", report.Tree)
	}
	lines := strings.Join(analysis.Lines(report, "Layers", true), "\n")
	for _, want := range []string{"worker / opencode-go / deepseek-v4.1-flash", "lead / openai-codex / gpt-6.2-sol", "50.0% (1/2)", "unreviewed 1", "unknown", "2.00s (measured)"} {
		if !strings.Contains(lines, want) {
			t.Fatal("missing layer output", want, lines)
		}
	}
	context := report.Tree.ContextMeasurements
	if context == nil || context.Partial || len(context.Sessions) != 8 {
		t.Fatal("context analysis lost archived coverage")
	}
	var archived, summaryTotal, rejected, machineChild bool
	for _, s := range context.Sessions {
		if s.Session == f.Workers["qwen-archived"] {
			for _, g := range s.Groups {
				if n := g.Numbers["within_input"]; g.Strategy == "summary" && g.Model == "qwen3.8-flash" && n.Value != nil && *n.Value == 2000 && n.Source == "measured" {
					archived = true
				}
			}
		}
		if s.Session == f.Lead {
			for _, g := range s.Groups {
				if n := g.Numbers["machine_child"]; n.Value != nil && *n.Value == 11 && n.Source == "derived" {
					machineChild = true
				}
			}
		}
	}
	for _, g := range context.Totals.Groups {
		if n := g.Numbers["within_input"]; g.Strategy == "summary" && g.Model == "qwen3.8-flash" && n.Value != nil && *n.Value == 2500 {
			summaryTotal = true
		}
	}
	for _, g := range context.Totals.Reviews {
		if n := g.Numbers["rejected_percent"]; g.Strategy == "three-layer" && g.Model == "deepseek-v4.1-flash" && n.Value != nil && *n.Value == 50 {
			rejected = true
		}
	}
	if !archived || !summaryTotal || !rejected || !machineChild {
		t.Fatalf("missing measurements: archived=%v summary total=%v rejection=%v machine=%v", archived, summaryTotal, rejected, machineChild)
	}
	for _, id := range f.Workers {
		if _, err := h.Attach(id); err == nil {
			t.Fatal("analysis resumed a grandchild", id)
		}
	}
	if _, err := r.Inspect(t.Context(), f.Workers["qwen-archived"], 0, 128); err == nil {
		t.Fatal("ordinary direct-child scope widened")
	}
}

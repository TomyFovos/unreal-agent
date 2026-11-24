package tui

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/analysis"
	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
	"github.com/unreallabsai/unreal-agent/internal/testsession"
)

type archiveAnalysisModel struct{}

func (archiveAnalysisModel) Respond(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
	return llm.Response{}, fmt.Errorf("fixture must not perform inference")
}

func TestAnalysisRestoresEndFromArchiveIndexWhenChildHasNoEndRecord(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sessions")
	id := testsession.WriteSettledChildren(t, dir, 3, 32)[2]
	path := filepath.Join(dir, string(id)+".session.jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cut := 1 + bytes.LastIndexByte(bytes.TrimSuffix(raw, []byte("\n")), '\n')
	if cut == 0 || !bytes.Contains(raw[cut:], []byte(`"Kind":"ended"`)) {
		t.Fatal("fixture should have one final end record")
	}
	if err := os.WriteFile(path, raw[:cut], 0600); err != nil {
		t.Fatal(err)
	}
	store, err := localfile.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	moved, err := store.ArchiveChild(t.Context(), localfile.ArchiveEntry{
		ChildID: id, ParentID: "main", OperationID: "spawn-0002", Status: operation.StatusCanceled,
	})
	if err != nil || !moved {
		t.Fatal(moved, err)
	}
	entry, err := store.ArchiveIndex(id)
	if err != nil || entry.EndedAt.IsZero() {
		t.Fatal("archive has no end timestamp", entry, err)
	}
	h, err := host.New(t.Context(), host.Config{Directory: dir, Build: func(ctx context.Context, _ session.ID) (host.Runtime, error) {
		return host.Runtime{Builder: contextbuilder.NewBuilder(), LLM: archiveAnalysisModel{}, Tools: tool.NewRegistry(tool.StaticTranslators{}), Operations: operation.NewLocalOperationManager(ctx)}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if _, err := h.Resume(t.Context(), host.Options{ID: "main"}); err != nil {
		t.Fatal(err)
	}
	reader := viewer.ParentReader{Host: h, ParentID: "main", Directory: dir}
	tree, err := analysis.ReadTree(t.Context(), "main", reader.InspectTree)
	if err != nil {
		t.Fatal(err)
	}
	report := tree.Report(entry.EndedAt.Add(time.Minute), nil)
	for _, child := range report.Dashboard.Children {
		if child.ID == id && child.End.Equal(entry.EndedAt) && child.Result == "canceled" {
			return
		}
	}
	t.Fatalf("archive end did not reach the child dashboard: %+v", report.Dashboard.Children)
}

func TestAnalyzeExportAndParentProjectionIdenticalAfterChildArchive(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sessions")
	children := testsession.WriteSettledChildren(t, dir, 6, 2048)
	store, err := localfile.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	var leases []*os.File
	for _, id := range children {
		lease, err := store.AcquireWriter(id)
		if err != nil {
			t.Fatal(err)
		}
		leases = append(leases, lease)
		defer lease.Close()
	}
	h, err := host.New(t.Context(), host.Config{Directory: dir, Build: func(ctx context.Context, _ session.ID) (host.Runtime, error) {
		return host.Runtime{Builder: contextbuilder.NewBuilder(), LLM: archiveAnalysisModel{}, Tools: tool.NewRegistry(tool.StaticTranslators{}), Operations: operation.NewLocalOperationManager(ctx)}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	owner, err := h.Resume(t.Context(), host.Options{ID: "main"})
	if err != nil {
		t.Fatal(err)
	}
	reader := viewer.ParentReader{Host: h, ParentID: "main", Directory: dir}
	now := time.Unix(1800000000, 0).UTC()
	project := func() ([]byte, []viewer.Row, host.View) {
		t.Helper()
		v, err := owner.Inspect(0, 10000)
		if err != nil || v.History.More {
			t.Fatal("incomplete parent projection", err)
		}
		m := NewModel("main")
		if err = m.apply(v); err != nil {
			t.Fatal(err)
		}
		client := viewer.NewClient(reader, viewer.New(viewer.SubagentOptions()), nil)
		if err = client.Refresh(t.Context(), "main"); err != nil {
			t.Fatal(err)
		}
		for _, id := range children {
			if err = client.Refresh(t.Context(), id); err != nil {
				t.Fatal("child inspection failed", id, err)
			}
		}
		rows := client.Model.Rows(now)
		report := m.Analysis(now, rows)
		if report.AgentCounts.Discovered != 6 || report.AgentCounts.Observed != 6 || report.AgentCounts.Success != 2 || report.AgentCounts.Failed != 2 || report.AgentCounts.Canceled != 2 {
			t.Fatal("analysis lost child results", report.AgentCounts)
		}
		raw, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		return raw, rows, v
	}
	before, beforeRows, beforeParent := project()
	export := func() string {
		t.Helper()
		_, items, _, err := store.ReadSnapshot(t.Context(), children[0])
		if err != nil {
			t.Fatal(err)
		}
		path, err := ExportConversation(t.Context(), reader, children[0], sessionstore.Sequence(len(items)), filepath.Join(t.TempDir(), "exports"))
		if err != nil {
			t.Fatal("archived child export failed", err)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var lines []string
		for _, line := range strings.Split(string(raw), "\n") {
			if !strings.HasPrefix(line, "Exported: ") {
				lines = append(lines, line)
			}
		}
		return strings.Join(lines, "\n")
	}
	beforeExport := export()
	for i, id := range children {
		if err = leases[i].Close(); err != nil {
			t.Fatal(err)
		}
		status := []operation.Status{operation.StatusCompleted, operation.StatusFailed, operation.StatusCanceled}[i%3]
		if moved, err := store.ArchiveChild(t.Context(), localfile.ArchiveEntry{ChildID: id, ParentID: "main", OperationID: operation.ID(fmt.Sprintf("spawn-%04d", i)), Status: status}); err != nil || !moved {
			t.Fatal(moved, err)
		}
	}
	after, afterRows, afterParent := project()
	if !bytes.Equal(before, after) {
		t.Fatalf("/analyze report changed after archive\nbefore %s\nafter  %s", before, after)
	}
	if !reflect.DeepEqual(beforeRows, afterRows) || !reflect.DeepEqual(beforeParent, afterParent) {
		t.Fatal("parent/child view projection changed after archive")
	}
	if beforeExport != export() {
		t.Fatal("/export changed child conversation after archive")
	}
	live, err := store.ListSessions(t.Context())
	if err != nil || len(live) != 1 || live[0].ID != "main" {
		t.Fatal("restore list includes archived children", live, err)
	}
}

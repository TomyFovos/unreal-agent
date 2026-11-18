package host_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"github.com/unreallabsai/unreal-agent/internal/testsession"
)

func TestRestoredParentProjectionUnchangedBySettledChildArchive(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sessions")
	children := testsession.WriteSettledChildren(t, dir, 6, 1024)
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
	project := func() host.View {
		t.Helper()
		h, err := host.New(t.Context(), host.Config{Directory: dir, Build: func(ctx context.Context, _ session.ID) (host.Runtime, error) {
			return host.Runtime{Builder: contextbuilder.NewBuilder(), LLM: archiveBenchmarkModel{}, Tools: tool.NewRegistry(tool.StaticTranslators{}), Operations: operation.NewLocalOperationManager(ctx)}, nil
		}})
		if err != nil {
			t.Fatal(err)
		}
		owner, err := h.Resume(t.Context(), host.Options{ID: "main"})
		if err != nil {
			h.Close()
			t.Fatal(err)
		}
		h.Close()
		v, err := owner.Inspect(0, 10000)
		if err != nil || v.History.More {
			t.Fatal("incomplete restore", err)
		}
		// Fresh ownership and the canonical host_start telemetry differ on
		// every restart, independently of archive. Compare all other fields.
		items := v.History.Items[:0:0]
		for _, item := range v.History.Items {
			if r, ok := item.Data.(sessionstore.HostRecord); ok && r.Activity != nil && r.Activity.Kind == "host_start" {
				continue
			}
			item.Sequence = sessionstore.Sequence(len(items) + 1)
			items = append(items, item)
		}
		v.History.Items, v.History.NextAfter = items, sessionstore.Sequence(len(items))
		return host.View{Session: v.Session, History: v.History, Operations: v.Operations, ProjectInstructions: v.ProjectInstructions}
	}
	before := project()
	for _, lease := range leases {
		if err := lease.Close(); err != nil {
			t.Fatal(err)
		}
	}
	for range 3 {
		after := project()
		if !reflect.DeepEqual(before, after) {
			t.Fatal("archive/repeated restore changed parent projection")
		}
	}
	live, err := store.ListSessions(t.Context())
	if err != nil || len(live) != 1 || live[0].ID != "main" {
		t.Fatal("restores did not settle children into archive", live, err)
	}
}

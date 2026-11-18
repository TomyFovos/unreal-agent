package subagent_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	sub "github.com/unreallabsai/unreal-agent/harness/subagent"
	"github.com/unreallabsai/unreal-agent/internal/testsession"
)

func TestRetainArchivedChildUsesIndexWithoutOpeningRecord(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sessions")
	ids := testsession.WriteSettledChildren(t, dir, 1, 1024)
	store, err := localfile.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	ops, err := store.Operations(t.Context(), "main")
	if err != nil || len(ops) != 1 {
		t.Fatal(ops, err)
	}
	if moved, err := store.ArchiveChild(t.Context(), localfile.ArchiveEntry{ChildID: ids[0], ParentID: "main", OperationID: ops[0].ID, Status: ops[0].Status}); err != nil || !moved {
		t.Fatal(moved, err)
	}
	path, err := store.ResolveSessionPath(ids[0])
	if err != nil {
		t.Fatal(err)
	}
	// Make just the transcript unavailable. Restoration must require only the
	// small location index; explicit history readers are tested separately.
	if err = os.Rename(path, path+".offline"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Rename(path+".offline", path); err != nil {
			t.Error(err)
		}
	}()
	manager, err := sub.NewManager(t.Context(), sub.Config{Owner: &host.Session{ID: "main"}, Directory: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	for range 3 {
		live, err := manager.RetainOperation(t.Context(), ops[0])
		if err != nil || live {
			t.Fatal("restore opened an archived transcript", live, err)
		}
	}
}

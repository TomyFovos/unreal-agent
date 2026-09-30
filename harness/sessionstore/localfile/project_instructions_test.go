package localfile

import (
	"errors"
	"io/fs"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/projectinstructions"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func TestCreateWithHostRecordsPersistsProjectInstructions(t *testing.T) {
	dir := t.TempDir()
	s := newHostTestStore(t, dir)
	snapshot, err := projectinstructions.FromContent([]byte("Use tabs."))
	if err != nil {
		t.Fatal(err)
	}
	record := sessionstore.HostRecord{Version: 1, Kind: sessionstore.HostProjectInstructions, ProjectInstructions: &snapshot}
	if _, err = s.CreateWithHostRecords(t.Context(), "bound", record); err != nil {
		t.Fatal(err)
	}
	// A fresh store decodes the snapshot from disk, content included.
	page, err := newHostTestStore(t, dir).Items(t.Context(), "bound", sessionstore.BeforeFirst, 16)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].Sequence != 1 {
		t.Fatalf("items = %+v", page.Items)
	}
	got := page.Items[0].Data.(sessionstore.HostRecord)
	if got.Kind != sessionstore.HostProjectInstructions || *got.ProjectInstructions != snapshot {
		t.Fatalf("record = %+v", got)
	}
}

func TestCreateWithHostRecordsRejectsInvalidRecordWithoutSession(t *testing.T) {
	s := newHostTestStore(t, t.TempDir())
	snapshot, err := projectinstructions.FromContent([]byte("A"))
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Content = "tampered"
	for _, record := range []sessionstore.HostRecord{
		{Version: 1, Kind: sessionstore.HostProjectInstructions, ProjectInstructions: &snapshot},
		{Version: 1, Kind: sessionstore.HostProjectInstructions},
		{Version: 1, Kind: "configuration", Configuration: []byte(`{}`), ProjectInstructions: &snapshot},
	} {
		if _, err = s.CreateWithHostRecords(t.Context(), "invalid", record); err == nil {
			t.Fatalf("accepted %+v", record)
		}
	}
	if _, err = s.Inspect(t.Context(), "invalid"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("invalid record created a session: %v", err)
	}
}

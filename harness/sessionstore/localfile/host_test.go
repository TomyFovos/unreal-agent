package localfile

import (
	"bytes"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"os"
	"path/filepath"
	"testing"
)

func TestHostReadsV2WithoutMigrationAndUnknownFails(t *testing.T) {
	dir := t.TempDir()
	s := newHostTestStore(t, dir)
	if _, err := s.Create(t.Context(), "old"); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendInput(t.Context(), "old", inbox.Input{ID: "input", Kind: inbox.InputExternal, Payload: []byte(`"hello"`)}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "old.session.jsonl")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	old := bytes.Replace(original, []byte(`"Version":3`), []byte(`"Version":2`), 1)
	if bytes.Equal(old, original) {
		t.Fatal("version replacement missed")
	}
	if err = os.WriteFile(path, old, 0600); err != nil {
		t.Fatal(err)
	}
	s = newHostTestStore(t, dir)
	lock, err := s.AcquireWriter("old")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, err = s.Resume(t.Context(), "old"); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path+".v2", []byte("unrelated old backup"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Resume(t.Context(), "old"); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, old) {
		t.Fatal("reading rewrote the v2 log", err)
	}
	if err = os.WriteFile(path, bytes.Replace(original, []byte(`"Version":3`), []byte(`"Version":999`), 1), 0600); err != nil {
		t.Fatal(err)
	}
	s = newHostTestStore(t, dir)
	if _, err = s.Resume(t.Context(), "old"); err == nil {
		t.Fatal("unknown version accepted")
	}
}

func newHostTestStore(t *testing.T, dir string) *Store {
	t.Helper()
	s, e := New(dir)
	if e != nil {
		t.Fatal(e)
	}
	return s
}

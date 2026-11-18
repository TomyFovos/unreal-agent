package localfile

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func archiveFixture(t *testing.T) (*Store, ArchiveEntry, []byte) {
	t.Helper()
	s, err := New(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	entry := ArchiveEntry{ChildID: "child", ParentID: "parent", OperationID: "start", Status: operation.StatusCompleted}
	if _, err = s.Create(t.Context(), entry.ChildID); err != nil {
		t.Fatal(err)
	}
	if err = s.AppendInput(t.Context(), entry.ChildID, inbox.Input{ID: "task", Kind: inbox.InputExternal, Payload: []byte(`"preserve every byte"`)}); err != nil {
		t.Fatal(err)
	}
	// An incomplete final line is also evidence; archive must not truncate it.
	f, err := os.OpenFile(s.sessionPath(entry.ChildID), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString(`{"incomplete":`)
	if err = errors.Join(err, f.Close()); err != nil {
		t.Fatal(err)
	}
	for _, relative := range []string{"operations/child/shell/out", "attachments/child/image.png", "operations/other/keep", "shared"} {
		path := filepath.Join(s.directory, relative)
		if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, []byte(relative), 0600); err != nil {
			t.Fatal(err)
		}
	}
	data, err := s.readRecord(entry.ChildID)
	if err != nil {
		t.Fatal(err)
	}
	return s, entry, data
}

func TestArchiveChildPreservesRecordsAssetsAndReaders(t *testing.T) {
	s, entry, before := archiveFixture(t)
	snapshot, items, ops, err := s.ReadSnapshot(t.Context(), entry.ChildID)
	if err != nil {
		t.Fatal(err)
	}
	moved, err := s.ArchiveChild(t.Context(), entry)
	if err != nil || !moved {
		t.Fatal(moved, err)
	}
	index, err := s.ArchiveIndex(entry.ChildID)
	if err != nil || index.ChildID != entry.ChildID || index.ParentID != entry.ParentID || index.OperationID != entry.OperationID || index.Status != entry.Status || index.EndedAt.IsZero() || index.ArchivedAt.IsZero() || index.OriginalPath != s.sessionPath(entry.ChildID) {
		t.Fatal("incomplete location index", index, err)
	}
	for _, relative := range index.Files {
		if _, err := os.Lstat(filepath.Join(s.directory, relative)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatal("file remained live", relative, err)
		}
		if _, err := os.Lstat(filepath.Join(s.archiveChildDirectory(index), relative)); err != nil {
			t.Fatal("archived asset missing", relative, err)
		}
	}
	for _, relative := range []string{"shared", "operations/other/keep"} {
		if _, err := os.Stat(filepath.Join(s.directory, relative)); err != nil {
			t.Fatal("moved a shared/other Session file", relative, err)
		}
	}
	fresh, err := New(s.directory)
	if err != nil {
		t.Fatal(err)
	}
	after, err := fresh.readRecord(entry.ChildID)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("archive rewrote or truncated canonical bytes", err)
	}
	gotSnapshot, gotItems, gotOps, err := fresh.ReadSnapshot(t.Context(), entry.ChildID)
	if err != nil || !reflect.DeepEqual(snapshot, gotSnapshot) || !reflect.DeepEqual(items, gotItems) || !reflect.DeepEqual(ops, gotOps) {
		t.Fatal("archive changed canonical snapshot", err)
	}
	if _, err = fresh.Resume(t.Context(), entry.ChildID); err != nil {
		t.Fatal("archived Resume lookup failed", err)
	}
	if _, err = fresh.RecoveryOperations(t.Context(), entry.ChildID); err != nil {
		t.Fatal("archived recovery lookup failed", err)
	}
	view, err := fresh.OpenRestoreView(t.Context(), entry.ChildID)
	if err != nil {
		t.Fatal(err)
	}
	page, err := view.Items(t.Context(), entry.ChildID, 0, 100)
	if err != nil || !reflect.DeepEqual(items, page.Items) {
		t.Fatal("archived history lookup failed", err)
	}
	if err = view.Close(); err != nil {
		t.Fatal(err)
	}
	live, err := fresh.ListSessions(t.Context())
	if err != nil || len(live) != 0 {
		t.Fatal("restore enumeration included archived records", live, err)
	}
	lease, err := fresh.AcquireWriter(entry.ChildID)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if _, err = s.AcquireWriter(entry.ChildID); !errors.Is(err, ErrWriterOwned) {
		t.Fatal("relocation changed writer ownership", err)
	}
	if _, err = os.Stat(s.sessionPath(entry.ChildID) + ".lock"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("writer recreated live lock", err)
	}
	for range 3 {
		if moved, err = fresh.ArchiveChild(t.Context(), entry); err != nil || !moved {
			t.Fatal("archive is not idempotent", moved, err)
		}
		if err = fresh.RecoverArchives(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestArchiveChildWaitsForExecutorLease(t *testing.T) {
	s, entry, before := archiveFixture(t)
	lease, err := s.AcquireWriter(entry.ChildID)
	if err != nil {
		t.Fatal(err)
	}
	moved, err := s.ArchiveChild(t.Context(), entry)
	if err != nil || moved {
		t.Fatal("archived a live executor", moved, err)
	}
	if _, err = s.ArchiveIndex(entry.ChildID); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("published an index for a live executor", err)
	}
	after, err := s.readRecord(entry.ChildID)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("changed a live executor record", err)
	}
	if err = lease.Close(); err != nil {
		t.Fatal(err)
	}
	if moved, err = s.ArchiveChild(t.Context(), entry); err != nil || !moved {
		t.Fatal("free lease was not archived", moved, err)
	}
}

func TestArchiveRecoveryAtEveryRenameBoundary(t *testing.T) {
	for boundary := range 5 {
		t.Run(string(rune('0'+boundary)), func(t *testing.T) {
			s, entry, before := archiveFixture(t)
			lease, err := s.AcquireWriter(entry.ChildID)
			if err != nil {
				t.Fatal(err)
			}
			lease.Close()
			entry.OriginalPath, entry.ArchivedAt, entry.EndedAt = s.sessionPath(entry.ChildID), time.Now().UTC(), time.Now().UTC()
			entry.Files = []string{sessionFilename(entry.ChildID) + ".lock", "operations/child", "attachments/child", sessionFilename(entry.ChildID)}
			for _, dir := range []string{filepath.Dir(s.archiveIndexPath(entry.ChildID, true)), filepath.Dir(s.archiveIndexPath(entry.ChildID, false)), s.archiveChildDirectory(entry)} {
				if err = os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			data, err := json.Marshal(entry)
			if err != nil {
				t.Fatal(err)
			}
			if err = publishFile(filepath.Dir(s.archiveIndexPath(entry.ChildID, true)), s.archiveIndexPath(entry.ChildID, true), data); err != nil {
				t.Fatal(err)
			}
			// Simulate process loss after durable index write and after each
			// subsequent rename. No record copy or duplicate is introduced.
			for _, relative := range entry.Files[:boundary] {
				target := filepath.Join(s.archiveChildDirectory(entry), relative)
				if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
					t.Fatal(err)
				}
				if err = os.Rename(filepath.Join(s.directory, relative), target); err != nil {
					t.Fatal(err)
				}
			}
			fresh, err := New(s.directory)
			if err != nil {
				t.Fatal(err)
			}
			assertReadable := func() {
				t.Helper()
				got, err := fresh.readRecord(entry.ChildID)
				if err != nil || !bytes.Equal(got, before) {
					t.Fatal("crash left child unreadable/changed", err)
				}
				places := 0
				for _, path := range []string{s.sessionPath(entry.ChildID), filepath.Join(s.archiveChildDirectory(entry), sessionFilename(entry.ChildID))} {
					if _, err := os.Stat(path); err == nil {
						places++
					}
				}
				if places != 1 {
					t.Fatal("record must exist in exactly one place", places)
				}
			}
			assertReadable()
			lease, err = fresh.AcquireWriter(entry.ChildID)
			if err != nil {
				t.Fatal("pending move lost writer-lock identity", err)
			}
			if other, err := s.AcquireWriter(entry.ChildID); !errors.Is(err, ErrWriterOwned) {
				if other != nil {
					other.Close()
				}
				t.Fatal("pending move admitted a second writer", err)
			}
			if err := fresh.RecoverArchives(t.Context()); err != nil {
				t.Fatal(err)
			}
			assertReadable()
			if err := lease.Close(); err != nil {
				t.Fatal(err)
			}
			for range 3 {
				if err := fresh.RecoverArchives(context.Background()); err != nil {
					t.Fatal(err)
				}
				assertReadable()
			}
			if _, err := os.Stat(s.archiveIndexPath(entry.ChildID, false)); err != nil {
				t.Fatal("recovery did not publish index", err)
			}
			if _, err := os.Stat(s.archiveIndexPath(entry.ChildID, true)); !errors.Is(err, fs.ErrNotExist) {
				t.Fatal("recovery left a pending transaction", err)
			}
		})
	}
}

func TestArchiveLookupSurvivesStateDirectoryRelocation(t *testing.T) {
	s, entry, before := archiveFixture(t)
	if moved, err := s.ArchiveChild(t.Context(), entry); err != nil || !moved {
		t.Fatal(moved, err)
	}
	root := filepath.Dir(s.directory)
	relocated := root + "-relocated"
	if err := os.Rename(root, relocated); err != nil {
		t.Fatal(err)
	}
	fresh, err := New(filepath.Join(relocated, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	after, err := fresh.readRecord(entry.ChildID)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("original_path metadata pinned lookups to the old state root", err)
	}
	index, err := fresh.ArchiveIndex(entry.ChildID)
	if err != nil || index.OriginalPath != s.sessionPath(entry.ChildID) {
		t.Fatal("relocation rewrote archive evidence", index, err)
	}
}

var _ sessionstore.Store = (*Store)(nil)

package localfile

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"golang.org/x/sys/unix"
)

// ArchiveEntry is a location index, never a replacement for canonical history.
// Files are relative to the live directory and retain their original names.
// An entry in pending/ is also readable; its renames are completed on startup.
type ArchiveEntry struct {
	Pending      bool             `json:"-"`
	ChildID      session.ID       `json:"child_id"`
	ParentID     session.ID       `json:"parent_id"`
	OperationID  operation.ID     `json:"operation_id"`
	EndedAt      time.Time        `json:"ended_at"`
	Status       operation.Status `json:"status"`
	ArchivedAt   time.Time        `json:"archived_at"`
	OriginalPath string           `json:"original_path"`
	Files        []string         `json:"files"`
}

func (s *Store) archiveDirectory() string { return s.directory + "-archive" }
func (s *Store) archiveIndexPath(id session.ID, pending bool) string {
	kind := "index"
	if pending {
		kind = "pending"
	}
	return filepath.Join(s.archiveDirectory(), kind, string(id)+".json")
}
func (s *Store) archiveChildDirectory(e ArchiveEntry) string {
	return filepath.Join(s.archiveDirectory(), string(e.ParentID), string(e.ChildID))
}

// ArchiveIndex resolves one child in constant time, without opening its record
// or enumerating any parent's history. Both sides of index publication are
// accepted so concurrent readers can cross the pending -> index rename.
func (s *Store) ArchiveIndex(id session.ID) (ArchiveEntry, error) {
	if err := validateSessionID(id); err != nil {
		return ArchiveEntry{}, err
	}
	for _, pending := range []bool{false, true, false} {
		data, err := os.ReadFile(s.archiveIndexPath(id, pending))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return ArchiveEntry{}, err
		}
		var entry ArchiveEntry
		if err = json.Unmarshal(data, &entry); err != nil {
			return entry, fmt.Errorf("read archive index %q: %w", id, err)
		}
		if entry.ChildID != id || validateSessionID(entry.ParentID) != nil || filepath.Base(entry.OriginalPath) != sessionFilename(id) {
			return entry, fmt.Errorf("archive index identity mismatch for %q", id)
		}
		entry.Pending = pending
		return entry, nil
	}
	return ArchiveEntry{}, fs.ErrNotExist
}

// OpenRecord opens a read-only inode before reading it. If a rename wins at
// the live path, the durable index locates that same inode in the archive.
func (s *Store) OpenRecord(id session.ID) (*os.File, error) {
	if err := validateSessionID(id); err != nil {
		return nil, err
	}
	f, err := os.Open(s.sessionPath(id))
	if !errors.Is(err, fs.ErrNotExist) {
		return f, err
	}
	e, err := s.ArchiveIndex(id)
	if err != nil {
		return nil, err
	}
	return os.Open(filepath.Join(s.archiveChildDirectory(e), sessionFilename(id)))
}

func (s *Store) readRecord(id session.ID) ([]byte, error) {
	f, err := s.OpenRecord(id)
	if err != nil {
		return nil, err
	}
	// Keep reading past the initial stat if a writer appends concurrently.
	// A size hint avoids reallocating and copying the growing JSONL buffer.
	var data bytes.Buffer
	if info, err := f.Stat(); err == nil && info.Size() <= int64(int(^uint(0)>>1))-bytes.MinRead {
		data.Grow(int(info.Size()) + bytes.MinRead)
	}
	_, err = data.ReadFrom(f)
	return data.Bytes(), errors.Join(err, f.Close())
}

// StatSession gives read caches the resolved inode identity, including during
// archival. Callers must not construct a live-only session filename themselves.
func (s *Store) StatSession(id session.ID) (os.FileInfo, error) {
	f, err := s.OpenRecord(id)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	return info, errors.Join(err, f.Close())
}

// ResolveSessionPath is for an owner that already holds the writer lease.
// Observers use OpenRecord/StatSession, which tolerate a concurrent rename.
func (s *Store) ResolveSessionPath(id session.ID) (string, error) {
	if err := validateSessionID(id); err != nil {
		return "", err
	}
	path := s.sessionPath(id)
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		return path, err
	}
	e, err := s.ArchiveIndex(id)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.archiveChildDirectory(e), sessionFilename(id)), nil
}

// layoutLock protects the stable writer-lock inode while it changes directory.
// AcquireWriter holds a shared layout lock until it owns the writer lease;
// archive/recovery hold an exclusive one through all renames. It is never moved.
func (s *Store) layoutLock(exclusive bool) (*os.File, error) {
	fd, err := unix.Open(filepath.Join(s.directory, ".archive.lock"), unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "session archive layout")
	mode := unix.LOCK_SH
	if exclusive {
		mode = unix.LOCK_EX
	}
	if err = unix.Flock(fd, mode); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// ArchiveChild is called only after canonical parent settlement has been
// established. The lease is checked here, at the actual move boundary. A busy
// executor leaves everything live; callers retry when it releases ownership.
func (s *Store) ArchiveChild(ctx context.Context, entry ArchiveEntry) (bool, error) {
	if err := context.Cause(ctx); err != nil {
		return false, err
	}
	if err := validateSessionID(entry.ChildID); err != nil {
		return false, err
	}
	if err := validateSessionID(entry.ParentID); err != nil {
		return false, err
	}
	layout, err := s.layoutLock(true)
	if err != nil {
		return false, err
	}
	defer layout.Close()
	if existing, err := s.ArchiveIndex(entry.ChildID); err == nil {
		return s.finishArchive(ctx, existing)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	if _, err = os.Stat(s.sessionPath(entry.ChildID)); err != nil {
		return false, err
	}
	lease, err := s.acquireWriter(entry.ChildID)
	if errors.Is(err, ErrWriterOwned) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer lease.Close()
	state, _, err := s.readState(ctx, entry.ChildID)
	if err != nil {
		return false, err
	}
	entry.EndedAt = state.Snapshot.Session.CreatedAt
	for _, item := range state.Items {
		if item.RecordedAt.After(entry.EndedAt) {
			entry.EndedAt = item.RecordedAt
		}
	}
	for i := len(state.Items) - 1; i >= 0; i-- {
		item := state.Items[i]
		if r, ok := item.Data.(sessionstore.HostRecord); ok {
			if r.Kind == sessionstore.HostEnded && r.End != nil {
				entry.EndedAt = r.End.EndedAt
				break
			}
			if r.Kind == "finish" {
				entry.EndedAt = item.RecordedAt
				break
			}
		}
	}
	entry.OriginalPath = s.sessionPath(entry.ChildID)
	entry.ArchivedAt = time.Now().UTC()
	// Only these namespaces are exclusively Session-owned. Workspace files,
	// shared state, credentials and other Sessions are never moved.
	for _, relative := range []string{sessionFilename(entry.ChildID) + ".lock", filepath.Join("operations", string(entry.ChildID)), filepath.Join("attachments", string(entry.ChildID)), sessionFilename(entry.ChildID)} {
		if _, err = os.Lstat(filepath.Join(s.directory, relative)); err == nil {
			entry.Files = append(entry.Files, relative)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return false, err
		}
	}
	for _, dir := range []string{s.archiveDirectory(), filepath.Join(s.archiveDirectory(), "index"), filepath.Join(s.archiveDirectory(), "pending"), filepath.Dir(s.archiveChildDirectory(entry)), s.archiveChildDirectory(entry)} {
		if err = os.MkdirAll(dir, 0700); err != nil {
			return false, err
		}
		if err = syncDirectory(filepath.Dir(dir)); err != nil {
			return false, err
		}
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return false, err
	}
	// Write-ahead location index is durable before the first rename. Keeping
	// the record rename last makes it the per-child visibility boundary.
	if err = publishFile(filepath.Join(s.archiveDirectory(), "pending"), s.archiveIndexPath(entry.ChildID, true), data); err != nil {
		return false, err
	}
	return s.moveArchive(ctx, entry)
}

func (s *Store) finishArchive(ctx context.Context, entry ArchiveEntry) (bool, error) {
	if _, err := os.Stat(s.archiveIndexPath(entry.ChildID, false)); err == nil {
		return true, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	lease, err := s.acquireWriter(entry.ChildID)
	if errors.Is(err, ErrWriterOwned) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer lease.Close()
	return s.moveArchive(ctx, entry)
}

func (s *Store) moveArchive(ctx context.Context, entry ArchiveEntry) (bool, error) {
	for _, relative := range entry.Files {
		if err := context.Cause(ctx); err != nil {
			return false, err
		}
		if !filepath.IsLocal(relative) {
			return false, fmt.Errorf("invalid archive file %q", relative)
		}
		source := filepath.Join(s.directory, relative)
		target := filepath.Join(s.archiveChildDirectory(entry), relative)
		_, sourceErr := os.Lstat(source)
		_, targetErr := os.Lstat(target)
		if errors.Is(sourceErr, fs.ErrNotExist) && targetErr == nil {
			if err := errors.Join(syncDirectory(filepath.Dir(source)), syncDirectory(filepath.Dir(target))); err != nil {
				return false, err
			}
			continue // A previous attempt already renamed this exact file.
		}
		if sourceErr != nil {
			return false, sourceErr
		}
		if !errors.Is(targetErr, fs.ErrNotExist) {
			return false, fmt.Errorf("archive target already exists: %s: %w", target, fs.ErrExist)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return false, err
		}
		if err := syncDirectory(s.archiveChildDirectory(entry)); err != nil {
			return false, err
		}
		if err := os.Rename(source, target); err != nil {
			return false, err
		}
		if err := errors.Join(syncDirectory(filepath.Dir(source)), syncDirectory(filepath.Dir(target))); err != nil {
			return false, err
		}
	}
	if err := os.Rename(s.archiveIndexPath(entry.ChildID, true), s.archiveIndexPath(entry.ChildID, false)); err != nil {
		return false, err
	}
	s.evictCachedWriteState(entry.ChildID)
	return true, errors.Join(syncDirectory(filepath.Join(s.archiveDirectory(), "pending")), syncDirectory(filepath.Join(s.archiveDirectory(), "index")))
}

// RecoverArchives reads only unfinished index entries, never archived records.
// Completed entries do not contribute to Host restoration/reconciliation cost.
func (s *Store) RecoverArchives(ctx context.Context) error {
	entries, err := os.ReadDir(filepath.Join(s.archiveDirectory(), "pending"))
	if errors.Is(err, fs.ErrNotExist) || len(entries) == 0 && err == nil {
		return nil
	}
	if err != nil {
		return err
	}
	layout, err := s.layoutLock(true)
	if err != nil {
		return err
	}
	defer layout.Close()
	for _, file := range entries {
		name, ok := strings.CutSuffix(file.Name(), ".json")
		if !ok || !file.Type().IsRegular() {
			continue
		}
		entry, err := s.ArchiveIndex(session.ID(name))
		if errors.Is(err, fs.ErrNotExist) {
			continue // Another Host recovered it before we acquired the lock.
		}
		if err != nil {
			return err
		}
		if _, err = s.finishArchive(ctx, entry); err != nil {
			return err
		}
	}
	return nil
}

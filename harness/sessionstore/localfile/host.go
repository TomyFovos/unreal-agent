package localfile

import (
	"context"
	"errors"
	"fmt"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"time"
)

var ErrWriterOwned = errors.New("session already has an active writer")

// AcquireWriter locks a stable sidecar inode, never the replaceable log inode.
// The private state directory must be owned by the harness user. The archive
// layout lock serializes relocation of this inode; it is never unlinked.
func (s *Store) AcquireWriter(id session.ID) (*os.File, error) {
	if err := validateSessionID(id); err != nil {
		return nil, err
	}
	layout, err := s.layoutLock(false)
	if err != nil {
		return nil, err
	}
	defer layout.Close()
	return s.acquireWriter(id)
}

// Caller holds the layout lock. A pending move may already have relocated the
// lock while the record is still live; choose the existing lock inode itself.
func (s *Store) acquireWriter(id session.ID) (*os.File, error) {
	path := s.sessionPath(id) + ".lock"
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		entry, err := s.ArchiveIndex(id)
		if err == nil {
			path = filepath.Join(s.archiveChildDirectory(entry), sessionFilename(id)+".lock")
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, fmt.Errorf("open writer lock: %w", err)
	}
	f := os.NewFile(uintptr(fd), "session writer lock")
	if err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, ErrWriterOwned
		}
		return nil, fmt.Errorf("lock session: %w", err)
	}
	return f, nil
}

func (s *Store) Operations(ctx context.Context, id session.ID) ([]operation.Operation, error) {
	state, _, err := s.readState(ctx, id)
	if err != nil {
		return nil, err
	}
	return state.Operations, nil
}

// CreateWithHostRecords publishes a new session whose first items are the
// given records, so a session never exists without its creation bindings.
func (s *Store) CreateWithHostRecords(ctx context.Context, id session.ID, records ...sessionstore.HostRecord) (sessionstore.Snapshot, error) {
	if err := validateSessionID(id); err != nil {
		return sessionstore.Snapshot{}, err
	}
	if err := context.Cause(ctx); err != nil {
		return sessionstore.Snapshot{}, err
	}
	now := time.Now().UTC()
	state := newStoredState(id, now)
	for _, r := range records {
		if err := r.Validate(); err != nil {
			return sessionstore.Snapshot{}, err
		}
		item := state.appendItem(sessionstore.ItemHostRecord, r, now)
		if err := state.observeItem(itemRecord{Item: item}); err != nil {
			return sessionstore.Snapshot{}, err
		}
		state.Items = append(state.Items, item)
	}
	if err := s.publishInitialState(state); err != nil {
		return sessionstore.Snapshot{}, fmt.Errorf("create session %q: %w", id, err)
	}
	for _, item := range state.Items {
		s.notifyObservers(id, item)
	}
	return state.Snapshot, nil
}

func (s *Store) AppendHostRecord(ctx context.Context, id session.ID, r sessionstore.HostRecord) error {
	if err := r.Validate(); err != nil {
		return err
	}
	head, size, err := s.loadWriteState(ctx, id)
	if err != nil {
		return err
	}
	item := head.appendItem(sessionstore.ItemHostRecord, r, time.Now().UTC())
	if err = s.append(id, head, size, recordItem, itemRecord{Item: item}); err != nil {
		return err
	}
	s.notifyObservers(id, item)
	return nil
}

package localfile

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"golang.org/x/sys/unix"
	"os"
	"time"
)

var ErrWriterOwned = errors.New("session already has an active writer")

// AcquireWriter locks a stable sidecar inode, never the replaceable log inode.
// The private state directory must be owned by the harness user. Never unlink
// lock files: unlinking an active lock would permit a second writer.
func (s *Store) AcquireWriter(id session.ID) (*os.File, error) {
	if err := validateSessionID(id); err != nil {
		return nil, err
	}
	fd, err := unix.Open(s.sessionPath(id)+".lock", unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
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

// Upgrade preserves the original v2 bytes in an exclusive backup before
// publishing a v3 header. Call explicitly, under the session writer lock.
// All history records and sequence numbers are preserved.
func (s *Store) Upgrade(ctx context.Context, id session.ID) error {
	if err := validateSessionID(id); err != nil {
		return err
	}
	if _, _, err := s.readState(ctx, id); err != nil {
		return err
	}
	raw, err := os.ReadFile(s.sessionPath(id))
	if err != nil {
		return err
	}
	first, rest, ok := bytes.Cut(raw, []byte{'\n'})
	if !ok {
		return fmt.Errorf("missing header")
	}
	var rec logRecord
	if err = json.Unmarshal(first, &rec); err != nil {
		return err
	}
	var header sessionRecord
	if err = json.Unmarshal(rec.Data, &header); err != nil {
		return err
	}
	if header.Version == formatVersion {
		return nil
	}
	if header.Version != 2 {
		return fmt.Errorf("unsupported migration from %d", header.Version)
	}
	backup := s.sessionPath(id) + ".v2"
	f, err := os.OpenFile(backup, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err == nil {
		if err = persistTemporary(f, raw); err != nil {
			return err
		}
		if err = syncDirectory(s.directory); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrExist) {
		return err
	} else {
		prior, e := os.ReadFile(backup)
		if e != nil {
			return e
		}
		if !bytes.Equal(prior, raw) {
			return fmt.Errorf("existing v2 backup differs; explicit migration required")
		}
	}
	header.Version = formatVersion
	prefix, err := encodeRecord(recordSession, header)
	if err != nil {
		return err
	}
	if err = publishFile(s.directory, s.sessionPath(id), append(prefix, rest...)); err != nil {
		return err
	}
	s.evictCachedWriteState(id)
	return nil
}

func (s *Store) Operations(ctx context.Context, id session.ID) ([]operation.Operation, error) {
	state, _, err := s.readState(ctx, id)
	if err != nil {
		return nil, err
	}
	return state.Operations, nil
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

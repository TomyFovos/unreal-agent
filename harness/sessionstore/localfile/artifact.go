package localfile

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"golang.org/x/sys/unix"
)

func openReadFile(ctx context.Context, path string, flags int) (*os.File, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	return permission.FromContext(ctx).OpenFile(path, os.O_RDONLY|flags, 0)
}

// CaptureArtifact records the bytes read from one opened file. The attachment
// is synced before the referring item commits, so an interrupted write cannot
// leave a committed locator pointing to an unpublished version.
// Caller holds the Session's normal writer ownership.
func (s *Store) CaptureArtifact(ctx context.Context, id session.ID, path string) (sessionstore.Item, error) {
	if !filepath.IsAbs(path) {
		return sessionstore.Item{}, fmt.Errorf("artifact source path must be absolute")
	}
	head, size, err := s.loadWriteState(ctx, id)
	if err != nil {
		return sessionstore.Item{}, err
	}
	file, err := openReadFile(ctx, path, unix.O_NONBLOCK)
	if err != nil {
		return sessionstore.Item{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return sessionstore.Item{}, err
	}
	if !info.Mode().IsRegular() {
		return sessionstore.Item{}, fmt.Errorf("artifact source is not a regular file")
	}
	record, err := s.writeArtifact(ctx, id, path, file)
	if err != nil {
		return sessionstore.Item{}, err
	}
	item := head.appendItem(sessionstore.ItemHostRecord, sessionstore.HostRecord{Version: 1, Kind: sessionstore.HostArtifact, Artifact: &record}, time.Now().UTC())
	if err := s.append(id, head, size, recordItem, itemRecord{Item: item}); err != nil {
		return sessionstore.Item{}, err
	}
	s.notifyObservers(id, item)
	return item, nil
}

type artifactReader struct {
	ctx  context.Context
	file *os.File
}

func (r artifactReader) Read(p []byte) (int, error) {
	if err := context.Cause(r.ctx); err != nil {
		return 0, err
	}
	return r.file.Read(p)
}

func (s *Store) writeArtifact(ctx context.Context, id session.ID, path string, source *os.File) (sessionstore.ArtifactRecord, error) {
	directory := filepath.Join(s.directory, "attachments", string(id))
	if err := os.MkdirAll(directory, 0700); err != nil {
		return sessionstore.ArtifactRecord{}, err
	}
	temp, err := os.CreateTemp(directory, ".artifact-*.tmp")
	if err != nil {
		return sessionstore.ArtifactRecord{}, err
	}
	defer os.Remove(temp.Name())
	digest := sha256.New()
	count, err := io.Copy(io.MultiWriter(temp, digest), artifactReader{ctx, source})
	if err != nil {
		temp.Close()
		return sessionstore.ArtifactRecord{}, err
	}
	if err := errors.Join(temp.Sync(), temp.Close()); err != nil {
		return sessionstore.ArtifactRecord{}, err
	}
	sum := fmt.Sprintf("%x", digest.Sum(nil))
	if err := os.Rename(temp.Name(), filepath.Join(directory, sum)); err != nil {
		return sessionstore.ArtifactRecord{}, err
	}
	if err := errors.Join(syncDirectory(directory), syncDirectory(filepath.Dir(directory)), syncDirectory(s.directory)); err != nil {
		return sessionstore.ArtifactRecord{}, err
	}
	return sessionstore.ArtifactRecord{Owner: id, Path: path, SHA256: sum, Size: count}, nil
}

func (s *Store) OpenArtifact(ctx context.Context, artifact sessionstore.ArtifactRecord) (*os.File, error) {
	if err := artifact.Validate(); err != nil {
		return nil, err
	}
	if err := validateSessionID(artifact.Owner); err != nil {
		return nil, err
	}
	if err := permission.FromContext(ctx).CheckPath(artifact.Path, false); err != nil {
		return nil, err
	}
	return s.openSessionSidecar(artifact.Owner, filepath.Join("attachments", string(artifact.Owner), artifact.SHA256), unix.O_NOFOLLOW|unix.O_NONBLOCK)
}

//go:build linux || darwin

package mutation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/unreallabsai/unreal-agent/harness/permission"
	"golang.org/x/sys/unix"
)

func (s *Service) captureIndependenceMetadata(ctx context.Context, path string) (independencePath, error) {
	return s.captureAuthorizedIndependence(ctx, path, false, false)
}

// Metadata for a different, historical target needs current read authority.
// A mutation's own revision observation uses its existing write authority.
// In either case the target is opened only through the policy-approved parent.
// The subsequent nofollow walk from / detects replacement of every ancestor;
// it supplies identity evidence, never a grant or a target I/O descriptor.
func (s *Service) captureAuthorizedIndependence(ctx context.Context, path string, read, write bool) (independencePath, error) {
	proof := independencePath{path: path}
	if ctx.Err() != nil || !filepath.IsAbs(path) || len(path) > 4096 {
		return proof, ErrIndependenceUnproven
	}
	if err := s.authorize(ctx, path, write); err != nil {
		return proof, err
	}
	approved, base, err := s.authorizedParent(ctx, path, write)
	if err != nil {
		return proof, err
	}
	defer approved.Close()
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) < 2 || len(parts) > 128 {
		return proof, ErrIndependenceUnproven
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return proof, ErrIndependenceUnproven
	}
	parent := os.NewFile(uintptr(fd), "/")
	defer func() { parent.Close() }()
	for i := 0; ; i++ {
		info, err := parent.Stat()
		if err != nil || ctx.Err() != nil {
			return proof, ErrIndependenceUnproven
		}
		proof.parents = append(proof.parents, info)
		if i == len(parts)-1 {
			break
		}
		fd, err := unix.Openat(int(parent.Fd()), parts[i], unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			return proof, ErrIndependenceUnproven
		}
		parent.Close()
		parent = os.NewFile(uintptr(fd), parts[i])
	}
	authorizedInfo, err := approved.Stat()
	if err != nil || len(proof.parents) == 0 || !os.SameFile(authorizedInfo, proof.parents[len(proof.parents)-1]) || checkFilesystem(approved) != nil {
		return proof, ErrIndependenceUnproven
	}
	if ctx.Err() != nil {
		return proof, ErrIndependenceUnproven
	}
	if err := permission.FromContext(ctx).CheckPath(path, write); err != nil {
		return proof, err
	}
	if s.observeIO != nil {
		s.observeIO(path, "metadata")
		if ctx.Err() != nil {
			return proof, ErrIndependenceUnproven
		}
	}
	// Never use the fresh pathname walk as a replacement for the grant.
	fd, err = unix.Openat(int(approved.Fd()), base, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if errors.Is(err, unix.ENOENT) {
		return proof, nil
	}
	if err != nil {
		return proof, ErrIndependenceUnproven
	}
	file := os.NewFile(uintptr(fd), parts[len(parts)-1])
	defer file.Close()
	var before, after unix.Stat_t
	if unix.Fstat(fd, &before) != nil || before.Mode&unix.S_IFMT != unix.S_IFREG || before.Nlink != 1 {
		return proof, ErrIndependenceUnproven
	}
	proof.size = before.Size
	if !read {
		proof.target, err = file.Stat()
		if err != nil {
			return proof, ErrIndependenceUnproven
		}
		return proof, nil
	}
	if ctx.Err() != nil {
		return proof, ErrIndependenceUnproven
	}
	if s.observeIO != nil {
		s.observeIO(path, "read")
		if ctx.Err() != nil {
			return proof, ErrIndependenceUnproven
		}
	}
	data, err := ReadBounded(file)
	if err != nil || ctx.Err() != nil || unix.Fstat(fd, &after) != nil || before.Dev != after.Dev || before.Ino != after.Ino || after.Nlink != 1 || before.Size != after.Size {
		return proof, ErrIndependenceUnproven
	}
	proof.target, err = file.Stat()
	if err != nil {
		return proof, ErrIndependenceUnproven
	}
	if s.observeIO != nil {
		s.observeIO(path, "hash")
	}
	proof.revision = RevisionOf(data)
	return proof, nil
}

//go:build linux || darwin

package mutation

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

func acquireLock(ctx context.Context, path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if err := checkFilesystem(file); err != nil {
		file.Close()
		return nil, err
	}
	for {
		if err = ctx.Err(); err != nil {
			file.Close()
			return nil, err
		}
		err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return file, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EINTR) {
			file.Close()
			return nil, err
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			file.Close()
			return nil, ctx.Err()
		}
	}
}
func (s *Service) openParent(path string) (*os.File, string, error) {
	rel, err := filepath.Rel(s.root, path)
	if err != nil {
		return nil, "", err
	}
	parts := strings.Split(rel, string(filepath.Separator))
	fd, err := unix.Open(s.root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, "", err
	}
	for _, part := range parts[:len(parts)-1] {
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		unix.Close(fd)
		if err != nil {
			return nil, "", err
		}
		fd = next
	}
	parent := os.NewFile(uintptr(fd), filepath.Dir(path))
	if err := checkFilesystem(parent); err != nil {
		parent.Close()
		return nil, "", err
	}
	return parent, parts[len(parts)-1], nil
}
func readTarget(parent *os.File, base string) ([]byte, bool, os.FileMode, error) {
	fd, err := unix.Openat(int(parent.Fd()), base, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, false, 0600, nil
	}
	if err != nil {
		return nil, false, 0, err
	}
	f := os.NewFile(uintptr(fd), base)
	defer f.Close()
	var st unix.Stat_t
	if err = unix.Fstat(fd, &st); err != nil {
		return nil, false, 0, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 {
		return nil, false, 0, errors.New("only regular single-link files are supported")
	}
	data, err := ReadBounded(f)
	return data, true, os.FileMode(st.Mode) & 0777, err
}
func replaceTarget(parent *os.File, base string, data []byte, mode os.FileMode) (bool, error) {
	name := ".unreal-mutation-" + fmt.Sprintf("%x", randomBytes())
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return false, err
	}
	file := os.NewFile(uintptr(fd), name)
	defer func() { file.Close(); unix.Unlinkat(int(parent.Fd()), name, 0) }()
	if _, err = file.Write(data); err != nil {
		return false, err
	}
	if err = file.Chmod(mode.Perm()); err != nil {
		return false, err
	}
	if err = file.Sync(); err != nil {
		return false, err
	}
	if err = file.Close(); err != nil {
		return false, err
	}
	if err = unix.Renameat(int(parent.Fd()), name, int(parent.Fd()), base); err != nil {
		return false, err
	}
	if err = parent.Sync(); err != nil {
		return true, err
	}
	return true, nil
}
func randomBytes() []byte {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

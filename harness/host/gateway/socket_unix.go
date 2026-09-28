//go:build linux || darwin

package gateway

import (
	"errors"
	"net"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func listen(path string) (net.Listener, func(), error) {
	if !filepath.IsAbs(path) || len(path) > 100 {
		return nil, nil, errors.New("gateway: absolute socket path of at most 100 bytes required")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, nil, errors.New("gateway: socket directory must be private (0700)")
	}
	parent, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	var parentStat unix.Stat_t
	err = unix.Fstat(parent, &parentStat)
	unix.Close(parent)
	if err != nil {
		return nil, nil, err
	}
	if parentStat.Uid != uint32(os.Geteuid()) {
		return nil, nil, errors.New("gateway: socket directory must belong to current user")
	}
	fd, err := unix.Open(path+".lock", unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, nil, err
	}
	lock := os.NewFile(uintptr(fd), path+".lock")
	var lockStat unix.Stat_t
	if err = unix.Fstat(fd, &lockStat); err != nil {
		lock.Close()
		return nil, nil, err
	}
	if lockStat.Mode&unix.S_IFMT != unix.S_IFREG || lockStat.Uid != uint32(os.Geteuid()) || lockStat.Nlink != 1 || lockStat.Mode&0077 != 0 {
		lock.Close()
		return nil, nil, errors.New("gateway: unsafe socket lock file")
	}

	if err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		lock.Close()
		return nil, nil, errors.New("gateway: socket already owned")
	}
	if old, e := os.Lstat(path); e == nil {
		if old.Mode()&os.ModeSocket == 0 {
			lock.Close()
			return nil, nil, errors.New("gateway: refusing to replace non-socket")
		}
		if e = os.Remove(path); e != nil {
			lock.Close()
			return nil, nil, e
		}
	} else if !os.IsNotExist(e) {
		lock.Close()
		return nil, nil, e
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		lock.Close()
		return nil, nil, err
	}
	if err = os.Chmod(path, 0600); err != nil {
		listener.Close()
		lock.Close()
		return nil, nil, err
	}
	return listener, func() { listener.Close(); lock.Close() }, nil
}

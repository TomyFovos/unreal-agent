//go:build linux || darwin

package testclaude

import (
	"errors"
	"os"
	"syscall"
)

// Diagnostics accept only a pinned regular file; opening an accidental FIFO is
// nonblocking and a symlink is never followed into unrelated test/Host state.
func openSyntheticProgress(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 256<<10 {
		f.Close()
		return nil, errors.New("synthetic progress file invalid")
	}
	return f, nil
}

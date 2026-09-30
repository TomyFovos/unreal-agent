//go:build linux || darwin

package projectinstructions

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Discover reads <workspace>/AGENTS.md through a descriptor pinned to the
// workspace directory. The operator-chosen workspace path may itself be a
// symlink; the instruction file may not be.
func Discover(workspace string) (Snapshot, error) {
	if !filepath.IsAbs(workspace) {
		return Snapshot{}, &Error{Code: CodeInvalidWorkspace, Path: workspace, Err: errors.New("workspace must be absolute")}
	}
	root, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return Snapshot{}, &Error{Code: CodeInvalidWorkspace, Path: workspace, Err: err}
	}
	dir, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return Snapshot{}, &Error{Code: CodeInvalidWorkspace, Path: workspace, Err: err}
	}
	defer unix.Close(dir)
	fd, err := unix.Openat(dir, FileName, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_NOCTTY, 0)
	switch {
	case errors.Is(err, unix.ENOENT):
		return None(), nil
	case errors.Is(err, unix.ELOOP):
		return Snapshot{}, &Error{Code: CodeSymlink, Path: FileName, Err: errors.New("symbolic links are not followed")}
	case err != nil:
		return Snapshot{}, &Error{Code: CodeUnreadable, Path: FileName, Err: err}
	}
	file := os.NewFile(uintptr(fd), FileName)
	defer file.Close()
	var st unix.Stat_t
	if err = unix.Fstat(fd, &st); err != nil {
		return Snapshot{}, &Error{Code: CodeUnreadable, Path: FileName, Err: err}
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return Snapshot{}, &Error{Code: CodeNotRegular, Path: FileName, Err: fmt.Errorf("file mode %#o is not a regular file", st.Mode&unix.S_IFMT)}
	}
	if st.Nlink != 1 {
		return Snapshot{}, &Error{Code: CodeNotRegular, Path: FileName, Err: errors.New("hard-linked files are not supported")}
	}
	if st.Size > MaxBytes {
		return Snapshot{}, &Error{Code: CodeOversized, Path: FileName, Err: fmt.Errorf("file exceeds %d bytes", MaxBytes)}
	}
	content, err := io.ReadAll(io.LimitReader(file, MaxBytes+1))
	if err != nil {
		return Snapshot{}, &Error{Code: CodeUnreadable, Path: FileName, Err: err}
	}
	return FromContent(content)
}

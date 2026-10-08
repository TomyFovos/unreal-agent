//go:build linux || darwin

package privateexport

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"uuid"

	"golang.org/x/sys/unix"
)

// Write and WriteStream share the same private, atomic publication contract.
func Write(ctx context.Context, dir, prefix, ext string, data []byte) (string, error) {
	return WriteStream(ctx, dir, prefix, ext, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
}

// WriteStream writes a private temporary file, then atomically publishes the
// completed inode without replacing an existing name. Filenames contain no
// session/user text. A failed write leaves no completed export.
func WriteStream(ctx context.Context, dir, prefix, ext string, write func(io.Writer) error) (string, error) {
	if prefix != "analysis" && prefix != "response" && prefix != "session" || ext != "json" && ext != "md" {
		return "", errors.New("unsupported private export type")
	}
	return writeStream(ctx, dir, prefix+"-"+uuid.New().String()+"."+ext, write)
}

// CreateRuntimeConfig publishes the normal launcher's nonsecret configuration
// once, using the same private, synced, no-replacement contract as exports.
func CreateRuntimeConfig(ctx context.Context, path string, data []byte) error {
	if filepath.Base(path) != "runtime.json" {
		return errors.New("unsupported runtime configuration filename")
	}
	// A crashed first run may leave its temporary name behind. A unique name
	// permits a later atomic creation without touching another writer's inode.
	_, err := writeStreamTemp(ctx, filepath.Dir(path), filepath.Base(path), ".runtime-"+uuid.New().String()+".tmp", func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
	return err
}

func writeStream(ctx context.Context, dir, name string, write func(io.Writer) error) (string, error) {
	return writeStreamTemp(ctx, dir, name, "."+name+".tmp", write)
}

func writeStreamTemp(ctx context.Context, dir, name, temp string, write func(io.Writer) error) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !filepath.IsAbs(dir) {
		return "", errors.New("export directory must be absolute")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", err
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err = unix.Fstat(fd, &st); err != nil {
		return "", err
	}
	if st.Uid != uint32(os.Geteuid()) || st.Mode&0777 != 0700 {
		return "", errors.New("export directory must be owned by current user with permissions 0700")
	}
	out, err := unix.Openat(fd, temp, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return "", err
	}
	f := os.NewFile(uintptr(out), temp)
	defer f.Close()
	defer unix.Unlinkat(fd, temp, 0)
	if err = write(contextWriter{ctx: ctx, Writer: f}); err != nil {
		return "", err
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}
	if err = f.Sync(); err != nil {
		return "", err
	}
	if err = f.Close(); err != nil {
		return "", err
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}
	// linkat atomically publishes the fully synced inode with no replacement
	// on Linux and macOS; remove its private temporary name afterwards.
	if err = unix.Linkat(fd, temp, fd, name, 0); err != nil {
		return "", err
	}
	committed := false
	defer func() {
		if !committed {
			_ = unix.Unlinkat(fd, name, 0)
		}
	}()
	if err = unix.Unlinkat(fd, temp, 0); err != nil {
		return "", err
	}
	if err = unix.Fsync(fd); err != nil {
		return "", err
	}
	committed = true
	return filepath.Join(dir, name), nil
}

type contextWriter struct {
	ctx context.Context
	io.Writer
}

func (w contextWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.Writer.Write(p)
}

func (w contextWriter) WriteString(s string) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return io.WriteString(w.Writer, s)
}

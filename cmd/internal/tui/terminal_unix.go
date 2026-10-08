//go:build linux || darwin

package tui

import (
	"context"
	"errors"
	"os"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// Terminal owns a separate /dev/tty descriptor so cancellation never closes the
// embedding application's stdin and raw mode is restored on every return path.
func Terminal(ctx context.Context, c Config) error {
	fd, err := unix.Open("/dev/tty", unix.O_RDWR|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), "/dev/tty")
	defer f.Close()
	state, err := term.MakeRaw(fd)
	if err != nil {
		return err
	}
	defer term.Restore(fd, state)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	keys := make(chan Key, 64)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer close(keys)
		var decoder Decoder
		buffer := make([]byte, 4096)
		defer clear(buffer)
		for ctx.Err() == nil {
			ready, err := unix.Poll([]unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}, 100)
			if errors.Is(err, unix.EINTR) {
				continue
			}
			if err != nil {
				return
			}
			if ready == 0 {
				for _, key := range decoder.FlushEscape() {
					select {
					case keys <- key:
					case <-ctx.Done():
						return
					}
				}
				continue
			}
			n, err := unix.Read(fd, buffer)
			if errors.Is(err, unix.EAGAIN) {
				continue
			}
			if err != nil || n == 0 {
				return
			}
			for _, key := range decoder.Feed(buffer[:n]) {
				select {
				case keys <- key:
				case <-ctx.Done():
					return
				}
			}
			clear(buffer[:n])
		}
	}()
	defer func() { cancel(); <-done }()
	c.Keys = keys
	if c.Theme == nil {
		theme := EnvironmentTheme(os.Getenv)
		c.Theme = &theme
	}
	c.Output = terminalWriter{ctx: ctx, fd: fd}
	c.Size = func() (int, int) {
		w, h, e := term.GetSize(fd)
		if e != nil {
			return 80, 24
		}
		return w, h
	}
	if _, err = c.Output.Write([]byte("\x1b[?1049h\x1b[?2004h")); err != nil {
		return err
	}
	defer func() {
		_, _ = (terminalWriter{ctx: context.Background(), fd: fd}).Write([]byte("\x1b[?2004l\x1b[?25h\x1b[?1049l"))
	}()
	return Run(ctx, c)
}

// Rendering has a bounded write wait and observes cancellation even when the
// terminal stops consuming output (for example, terminal flow control).
type terminalWriter struct {
	ctx context.Context
	fd  int
}

func (w terminalWriter) Write(data []byte) (int, error) {
	written := 0
	deadline := time.Now().Add(2 * time.Second)
	for written < len(data) {
		if err := w.ctx.Err(); err != nil {
			return written, err
		}
		if time.Now().After(deadline) {
			return written, os.ErrDeadlineExceeded
		}
		n, err := unix.Write(w.fd, data[written:])
		if n > 0 {
			written += n
		}
		if err == nil {
			continue
		}
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if !errors.Is(err, unix.EAGAIN) {
			return written, err
		}
		if _, err = unix.Poll([]unix.PollFd{{Fd: int32(w.fd), Events: unix.POLLOUT}}, 100); err != nil && !errors.Is(err, unix.EINTR) {
			return written, err
		}
	}
	return written, nil
}

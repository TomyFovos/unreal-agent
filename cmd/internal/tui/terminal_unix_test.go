//go:build linux || darwin

package tui

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestTerminalWriteCancellationWhileOutputBlocked(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	fd := int(writer.Fd())
	if err = unix.SetNonblock(fd, true); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 65536)
	for {
		_, err = unix.Write(fd, buffer)
		if errors.Is(err, unix.EAGAIN) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, err := (terminalWriter{ctx: ctx, fd: fd}).Write([]byte("waiting")); done <- err }()
	cancel()
	select {
	case err = <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked terminal write ignored cancellation")
	}
}

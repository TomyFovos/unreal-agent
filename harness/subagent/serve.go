package subagent

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"sync"
	"time"
)

// Serve binds one stdio connection to one parent-child relation. Closing stdin
// is owner loss: the child closes its runtime and releases the session lock.
// stdout is protocol-only; callers must send diagnostics to stderr.
func Serve(ctx context.Context, input io.ReadCloser, output io.WriteCloser, factory ChildFactory) error {
	// Standard streams can be non-pollable blocking descriptors in Go. Duplicate
	// them as nonblocking files so cancellation really interrupts pending reads.
	if f, ok := input.(*os.File); ok {
		pipe, err := interruptibleFile(f)
		if err != nil {
			return err
		}
		defer f.Close()
		input = pipe
	}
	if f, ok := output.(*os.File); ok {
		pipe, err := interruptibleFile(f)
		if err != nil {
			return err
		}
		defer f.Close()
		output = pipe
	}
	ctx, cancel := context.WithCancel(ctx)
	stopClose := context.AfterFunc(ctx, func() { _ = input.Close(); _ = output.Close() })
	var workers sync.WaitGroup
	defer func() { cancel(); _ = input.Close(); _ = output.Close(); stopClose(); workers.Wait() }()
	hello, err := readFrame(input)
	if err != nil {
		return err
	}
	if hello.Kind != "hello" || hello.Config == nil {
		return fmt.Errorf("expected child handshake")
	}
	config := *hello.Config
	if err = config.Validate(); err != nil {
		return err
	}
	if hello.Sender != string(config.ParentID) || hello.Target != string(config.ChildID) {
		return fmt.Errorf("handshake identity mismatch")
	}
	if factory == nil {
		return fmt.Errorf("child factory is required")
	}
	var writeMu sync.Mutex
	ep := &endpoint{ctx: ctx, connection: hello.Connection, local: hello.Target, remote: hello.Sender}
	ep.write = func(writeContext context.Context, f frame) error {
		stop := context.AfterFunc(writeContext, cancel)
		defer stop()
		data, err := encodeFrame(f)
		if err != nil {
			return err
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		for len(data) > 0 {
			n, err := output.Write(data)
			if err != nil {
				return err
			}
			if n == 0 {
				return io.ErrShortWrite
			}
			data = data[n:]
		}
		return nil
	}
	var child *host.Session
	ready := make(chan struct{})
	ep.receive = func(ctx context.Context, in inbox.Input) (host.Receipt, error) {
		select {
		case <-ready:
		case <-ctx.Done():
			return host.Receipt{}, context.Cause(ctx)
		}
		if in.Kind == inbox.InputControl {
			control, err := in.DecodeControlMessage()
			if err != nil || control.Mode != inbox.StopHard {
				return host.Receipt{}, fmt.Errorf("unsupported parent control")
			}
			return child.Submit(ctx, child.Generation, in)
		}
		return child.SubmitPeer(ctx, child.Generation, string(config.ParentID), string(config.OperationID), in)
	}
	readDone := make(chan error, 1)
	workers.Go(func() {
		for {
			f, err := readFrame(input)
			if err == nil {
				err = ep.accept(f)
			}
			if err != nil {
				readDone <- err
				return
			}
		}
	})
	var closer io.Closer
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for {
		child, closer, err = factory(ctx, config, ep.send)
		if !errors.Is(err, localfile.ErrWriterOwned) {
			break
		}
		if closer != nil {
			_ = closer.Close()
		}
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-deadline.C:
			return err
		case <-time.After(50 * time.Millisecond):
		}
	}
	if err != nil {
		return err
	}
	if closer == nil {
		return fmt.Errorf("child factory must provide runtime owner closer")
	}
	defer func() { cancel(); _ = closer.Close() }()
	if child == nil || child.ID != config.ChildID {
		return fmt.Errorf("child factory identity mismatch")
	}
	close(ready)
	welcome := ep.envelope("welcome")
	if err = ep.write(ctx, welcome); err != nil {
		return err
	}
	// Ready has stable identity in canonical ChildConfig and is retryable after
	// either side loses an ACK. Final completion never travels as a Peer input.
	readyDone := make(chan error, 1)
	workers.Go(func() {
		payload, _ := json.Marshal(inbox.PeerMessage{Version: 1, Sender: string(config.ChildID), Target: string(config.ParentID), Handle: string(config.OperationID), Kind: "ready", Text: "Child is ready; operation handle " + string(config.OperationID)})
		_, err := ep.send(ctx, inbox.Input{ID: config.ReadyID, Kind: inbox.InputPeer, Payload: payload})
		readyDone <- err
	})
	for {
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case err := <-readDone:
			return err
		case err := <-readyDone:
			if err != nil {
				return err
			}
			readyDone = nil
		case <-child.Done():
			// Even a very fast Finish must not race away the stable ready intent.
			if readyDone != nil {
				select {
				case err := <-readyDone:
					if err != nil {
						return err
					}
				case err := <-readDone:
					return err
				case <-ctx.Done():
					return context.Cause(ctx)
				}
			}
			if result := child.Finish(); result != nil {
				f := ep.envelope("finish")
				f.Finish = result
				return ep.write(ctx, f)
			}
			return child.Wait(ctx)
		}
	}
}

func interruptibleFile(f *os.File) (*os.File, error) {
	fd, err := unix.Dup(int(f.Fd()))
	if err != nil {
		return nil, err
	}
	unix.CloseOnExec(fd)
	if err = unix.SetNonblock(fd, true); err != nil {
		unix.Close(fd)
		return nil, err
	}
	return os.NewFile(uintptr(fd), "subagent protocol"), nil
}

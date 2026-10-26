package subagent

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/permission"
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
	if hello.Mode != "" && !hello.Mode.Valid() {
		return fmt.Errorf("invalid parent permission mode")
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
	var control *permission.Controller // nil for a child with a fixed policy
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
	// The parent's mode and answers apply to this child's controller.
	parentControl := func(f frame) error {
		if f.Connection != ep.connection || f.Sender != ep.remote || f.Target != ep.local {
			return fmt.Errorf("stale or mismatched peer channel")
		}
		select {
		case <-ready:
		case <-ctx.Done():
			return context.Cause(ctx)
		}
		switch {
		case f.Kind == "mode" && f.Mode.Valid():
			if control != nil {
				_ = control.Follow(f.Mode) // a stopping child refuses; nothing to apply
			}
			return nil
		case f.Kind == "decide" && f.Approval != nil:
			// A request that ended meanwhile reports approval_done instead.
			if control != nil {
				_ = control.Answer(f.Approval.ID, f.Approval.Allow)
			}
			return nil
		}
		return fmt.Errorf("invalid parent control frame")
	}
	readDone := make(chan error, 1)
	workers.Go(func() {
		for {
			f, err := readFrame(input)
			if err == nil {
				if f.Kind == "mode" || f.Kind == "decide" {
					err = parentControl(f)
				} else {
					err = ep.accept(f)
				}
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
		child, closer, err = factory(ctx, config, ChildLink{Send: ep.send, Mode: hello.Mode})
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
	if hello.Mode != "" {
		// A child runtime without modes keeps its fixed policy.
		control, _ = child.PermissionControl(child.Generation)
	}
	close(ready)
	welcome := ep.envelope("welcome")
	if err = ep.write(ctx, welcome); err != nil {
		return err
	}
	if control != nil {
		workers.Go(func() { forwardApprovals(ctx, ep, control) })
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

// forwardApprovals tells the parent which of this child's requests wait for
// its decision (U17), and which ended.
func forwardApprovals(ctx context.Context, ep *endpoint, control *permission.Controller) {
	sent := map[string]bool{}
	for {
		snapshot, changed := control.Watch()
		waiting := map[string]bool{}
		for _, p := range snapshot.Pending {
			waiting[p.ID] = true
			if !sent[p.ID] {
				f := ep.envelope("approval")
				f.Approval = &Approval{ID: p.ID, Tool: p.Tool, Scope: p.Scope}
				if ep.write(ctx, f) != nil {
					return
				}
				sent[p.ID] = true
			}
		}
		for id := range sent {
			if !waiting[id] {
				f := ep.envelope("approval_done")
				f.Approval = &Approval{ID: id}
				if ep.write(ctx, f) != nil {
					return
				}
				delete(sent, id)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-changed:
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

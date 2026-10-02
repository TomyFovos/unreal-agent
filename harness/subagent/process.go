package subagent

import (
	"context"
	"fmt"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/primitives"
	"sync"
	"uuid"
)

type processChannel struct {
	ctx       context.Context
	cancel    context.CancelFunc
	endpoint  *endpoint
	process   *primitives.ProcessInvocation
	connected chan struct{}
	done      chan struct{}
	err       error // written before done closes
	writeMu   sync.Mutex
}

func startProcess(ctx context.Context, c Config, child ChildConfig) *processChannel {
	ctx, cancel := context.WithCancel(ctx)
	p := &processChannel{ctx: ctx, cancel: cancel, connected: make(chan struct{}), done: make(chan struct{})}
	events := make(chan primitives.PrimitiveEvent, 16)
	p.process = primitives.StartProcess(ctx, primitives.ProcessStartRequest{Source: "subagent", CorrelationID: primitives.CorrelationID(uuid.New().String()), Path: c.Binary, Arguments: append([]string(nil), c.Arguments...), Directory: child.Workspace, Environment: append([]string(nil), c.Environment...), Pipes: primitives.ProcessPipeAll}, events)
	ep := &endpoint{ctx: ctx, connection: uuid.New().String(), local: string(child.ParentID), remote: string(child.ChildID)}
	ep.write = p.write
	ep.receive = func(ctx context.Context, input inbox.Input) (host.Receipt, error) {
		return c.Owner.SubmitPeer(ctx, c.Owner.Generation, string(child.ChildID), string(child.OperationID), input)
	}
	p.endpoint = ep
	go func() {
		defer close(p.done)
		defer cancel()
		decoder := frameDecoder{}
		welcomed := false
		draining := false
		for event := range events {
			switch event.Type {
			case primitives.PrimitiveEventProcessOutput:
				result := event.Result.(primitives.ProcessOutputResult)
				if result.Stream != primitives.ProcessStdout || draining {
					continue
				}
				frames, err := decoder.push(result.Data)
				if err == nil {
					for _, f := range frames {
						if f.Connection != ep.connection || f.Sender != ep.remote || f.Target != ep.local {
							err = fmt.Errorf("stale or mismatched child frame")
							break
						}
						switch f.Kind {
						case "welcome":
							if welcomed {
								err = fmt.Errorf("duplicate welcome")
							} else {
								welcomed = true
								close(p.connected)
							}
						case "finish":
							if f.Finish == nil {
								err = fmt.Errorf("missing finish record")
							} else {
								err = f.Finish.Validate()
							}
							// Canonical disk state, never this notification, determines completion.
						default:
							err = ep.accept(f)
						}
						if err != nil {
							break
						}
					}
				}
				if err != nil {
					p.err = err
					draining = true
					cancel()
				}
			case primitives.PrimitiveEventProcessStreamFailed:
				p.err = fmt.Errorf("child protocol stream failed")
				draining = true
				cancel()
			case primitives.PrimitiveEventFailed:
				p.err = fmt.Errorf("child process failed")
				return
			case primitives.PrimitiveEventCanceled:
				if p.err == nil {
					p.err = context.Cause(ctx)
				}
				return
			case primitives.PrimitiveEventProcessExited:
				if len(decoder.data) != 0 && p.err == nil {
					p.err = fmt.Errorf("truncated child frame")
				}
				result := event.Result.(primitives.ProcessExitResult)
				if result.ExitCode != 0 && p.err == nil {
					p.err = fmt.Errorf("child process exited without success")
				}
				return
			}
		}
	}()
	go func() {
		hello := ep.envelope("hello")
		hello.Config = &child
		if err := p.write(ctx, hello); err != nil {
			cancel()
		}
	}()
	return p
}
func (p *processChannel) write(ctx context.Context, f frame) error {
	// Cancellation of a pipe write after it started requires closing the channel.
	// Tear down that child rather than leave an unbounded writer behind.
	stop := context.AfterFunc(ctx, p.cancel)
	defer stop()
	data, err := encodeFrame(f)
	if err != nil {
		return err
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if err = context.Cause(p.ctx); err != nil {
		return err
	}
	events := make(chan primitives.PrimitiveEvent, 1)
	p.process.WriteInput(p.ctx, primitives.ProcessWriteRequest{Source: "subagent", CorrelationID: primitives.CorrelationID(uuid.New().String()), Data: data}, events)
	// Process cancellation closes the pipe even after a write began. Always
	// consume its completion before dispatching another write.
	event := <-events
	if event.Type != primitives.PrimitiveEventProcessInputWritten {
		return fmt.Errorf("child pipe write failed")
	}
	return nil
}
func (p *processChannel) send(ctx context.Context, input inbox.Input) (host.Receipt, error) {
	select {
	case <-p.connected:
	case <-p.done:
		return host.Receipt{}, fmt.Errorf("child channel closed")
	case <-ctx.Done():
		return host.Receipt{}, context.Cause(ctx)
	}
	return p.endpoint.send(ctx, input)
}

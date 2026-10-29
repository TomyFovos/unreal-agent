package subagent

import (
	"context"
	"fmt"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/primitives"
	"strings"
	"sync"
	"uuid"
)

type processChannel struct {
	ctx        context.Context
	cancel     context.CancelFunc
	endpoint   *endpoint
	process    *primitives.ProcessInvocation
	connected  chan struct{}
	finishHint chan struct{}
	// approvals carries the child's requests and their ends to the
	// manager, in order.
	approvals chan approvalEvent
	stderr    []byte // the end of the child's diagnostics, for failures
	done      chan struct{}
	err       error // written before done closes
	writeMu   sync.Mutex
}

type approvalEvent struct {
	approval Approval
	done     bool
}

// startProcess starts the child agent process under ctx's policy and tells
// it the parent's mode.
func startProcess(ctx context.Context, c Config, child ChildConfig, mode permission.Mode) *processChannel {
	ctx, cancel := context.WithCancel(ctx)
	p := &processChannel{ctx: ctx, cancel: cancel, connected: make(chan struct{}), finishHint: make(chan struct{}, 1), done: make(chan struct{}), approvals: make(chan approvalEvent, 64)}
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
		finishNotified := false
		for event := range events {
			switch event.Type {
			case primitives.PrimitiveEventProcessOutput:
				result := event.Result.(primitives.ProcessOutputResult)
				if result.Stream == primitives.ProcessStderr {
					p.stderr = append(p.stderr, result.Data...)
					if len(p.stderr) > 8<<10 {
						p.stderr = append([]byte(nil), p.stderr[len(p.stderr)-4<<10:]...)
					}
					continue
				}
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
						case "approval", "approval_done":
							if f.Approval == nil || f.Approval.ID == "" {
								err = fmt.Errorf("invalid child approval frame")
								break
							}
							select {
							case p.approvals <- approvalEvent{approval: *f.Approval, done: f.Kind == "approval_done"}:
							case <-ctx.Done():
							}
						case "finish":
							if f.Finish == nil {
								err = fmt.Errorf("missing finish record")
							} else {
								err = f.Finish.Validate()
							}
							// This is only a wakeup. The manager must verify durable
							// canonical Finish and terminal child Operations before
							// it starts owned-channel teardown and process reap.
							if err == nil && !finishNotified {
								finishNotified = true // At most one disk reconciliation per exact channel.
								select {
								case p.finishHint <- struct{}{}:
								default:
								}
							}
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
				p.err = fmt.Errorf("child process failed%s", p.diagnostics())
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
					p.err = fmt.Errorf("child process exited with code %d%s", result.ExitCode, p.diagnostics())
				}
				return
			}
		}
	}()
	go func() {
		hello := ep.envelope("hello")
		hello.Config = &child
		hello.Mode = mode
		if err := p.write(ctx, hello); err != nil {
			cancel()
		}
	}()
	return p
}

// diagnostics is the last lines the child wrote to stderr. The process
// goroutine owns p.stderr.
func (p *processChannel) diagnostics() string {
	text := strings.TrimSpace(strings.ToValidUTF8(string(p.stderr), "\uFFFD"))
	if text == "" {
		return ""
	}
	if lines := strings.Split(text, "\n"); len(lines) > 8 {
		text = strings.Join(lines[len(lines)-8:], "\n")
	}
	return ": " + text
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

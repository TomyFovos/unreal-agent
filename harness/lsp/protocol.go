// Package lsp owns language-server processes and safe semantic operations.
package lsp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/primitives"
)

const maxMessageBytes = 4 << 20

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string           { return "lsp " + e.Code + ": " + e.Message }
func failure(code, message string) error { return &Error{code, message} }
func errorCode(err error) string {
	if p := permission.Failure(err); p != nil {
		return string(p.Code)
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "canceled"
	}
	return "failed"
}

type envelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}
type reply struct {
	data json.RawMessage
	err  error
}
type diagnostics struct {
	Version     *int            `json:"version,omitempty"`
	URI         string          `json:"uri"`
	Diagnostics json.RawMessage `json:"diagnostics"`
}

type connection struct {
	ctx                context.Context
	cancel             context.CancelFunc
	process            *primitives.ProcessInvocation
	events             chan primitives.PrimitiveEvent
	done               chan struct{}
	ready              chan error
	sendGate           chan struct{}
	mu                 sync.Mutex
	pending            map[string]chan reply
	diagnostics        map[string]diagnostics
	diagnosticsChanged chan struct{}
	err                error
	replies            chan envelope
	next               atomic.Uint64
}

func startConnection(ctx context.Context, cfg ServerConfig, workspace string) (*connection, error) {
	if err := permission.FromContext(ctx).CheckProcess(); err != nil {
		return nil, err
	}
	life, cancel := context.WithCancel(ctx)
	c := &connection{ctx: life, cancel: cancel, events: make(chan primitives.PrimitiveEvent), done: make(chan struct{}), ready: make(chan error, 1), pending: make(map[string]chan reply), diagnostics: make(map[string]diagnostics), diagnosticsChanged: make(chan struct{}, 1), replies: make(chan envelope, 16), sendGate: make(chan struct{}, 1)}
	c.process = primitives.StartProcess(life, primitives.ProcessStartRequest{Source: "lsp", CorrelationID: "start", Path: cfg.Path, Arguments: append([]string(nil), cfg.Arguments...), Directory: workspace, Environment: append([]string(nil), cfg.Environment...), Pipes: primitives.ProcessPipeAll}, c.events)
	go c.readLoop()
	go c.replyLoop()
	select {
	case err := <-c.ready:
		if err != nil {
			cancel()
			<-c.done
			return nil, err
		}
		return c, nil
	case <-ctx.Done():
		cancel()
		<-c.done
		return nil, ctx.Err()
	}
}
func (c *connection) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err == nil {
		c.err = err
	}
	for key, ch := range c.pending {
		ch <- reply{err: c.err}
		delete(c.pending, key)
	}
}
func (c *connection) readLoop() {
	defer close(c.done)
	defer c.cancel()
	var buffer []byte
	started := false
	for {
		event := <-c.events
		switch event.Type {
		case primitives.PrimitiveEventProcessStarted:
			started = true
			c.ready <- nil
		case primitives.PrimitiveEventProcessOutput:
			output, ok := event.Result.(primitives.ProcessOutputResult)
			if !ok || output.Stream != primitives.ProcessStdout {
				continue
			}
			buffer = append(buffer, output.Data...)
			for {
				body, n, err := frame(buffer)
				if err != nil {
					c.fail(err)
					c.cancel()
					buffer = nil
					break
				}
				if n == 0 {
					break
				}
				buffer = buffer[n:]
				if err = c.receive(body); err != nil {
					c.fail(err)
					c.cancel()
					buffer = nil
					break
				}
			}
		case primitives.PrimitiveEventProcessStreamFailed:
			c.fail(failure("unavailable", "language server output failed"))
			c.cancel()
		case primitives.PrimitiveEventProcessExited, primitives.PrimitiveEventFailed, primitives.PrimitiveEventCanceled:
			err := failure("unavailable", "language server exited")
			if result, ok := event.Result.(primitives.PrimitiveFailureResult); ok && result.Denial != nil {
				err = result.Denial
			}
			c.fail(err)
			if !started {
				c.ready <- err
			}
			return
		}
	}
}
func frame(buffer []byte) ([]byte, int, error) {
	boundary := bytes.Index(buffer, []byte("\r\n\r\n"))
	if boundary < 0 {
		if len(buffer) > 8192 {
			return nil, 0, failure("protocol", "oversized LSP header")
		}
		return nil, 0, nil
	}
	if boundary > 8192 {
		return nil, 0, failure("protocol", "oversized LSP header")
	}
	length := -1
	for _, line := range strings.Split(string(buffer[:boundary]), "\r\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return nil, 0, failure("protocol", "invalid LSP header")
		}
		if strings.EqualFold(key, "Content-Type") {
			_, parameters, err := mime.ParseMediaType(strings.TrimSpace(value))
			charset := strings.ToLower(parameters["charset"])
			if err != nil || (charset != "" && charset != "utf-8" && charset != "utf8") {
				return nil, 0, failure("protocol", "unsupported LSP content encoding")
			}
		}
		if strings.EqualFold(key, "Content-Length") {
			n, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil || n < 0 || n > maxMessageBytes || length != -1 {
				return nil, 0, failure("protocol", "invalid LSP content length")
			}
			length = n
		}
	}
	if length < 0 {
		return nil, 0, failure("protocol", "missing LSP content length")
	}
	end := boundary + 4 + length
	if len(buffer) < end {
		return nil, 0, nil
	}
	return buffer[boundary+4 : end], end, nil
}
func (c *connection) receive(body []byte) error {
	var msg envelope
	if json.Unmarshal(body, &msg) != nil || msg.JSONRPC != "2.0" {
		return failure("protocol", "invalid JSON-RPC response")
	}
	if msg.Method != "" {
		if msg.Method == "textDocument/publishDiagnostics" {
			var value diagnostics
			if json.Unmarshal(msg.Params, &value) != nil {
				return failure("protocol", "invalid diagnostics")
			}
			c.mu.Lock()
			if len(c.diagnostics) < 64 || c.diagnostics[value.URI].URI != "" {
				if len(value.Diagnostics) > MaxResultBytes {
					value.Diagnostics = nil
				}
				c.diagnostics[value.URI] = value
				select {
				case c.diagnosticsChanged <- struct{}{}:
				default:
				}
			}
			c.mu.Unlock()
		}
		if len(msg.ID) > 0 {
			// No server-initiated applyEdit, executeCommand, dynamic registration or
			// hidden requests. A small bounded response refuses unsupported methods.
			raw := append(json.RawMessage(nil), msg.ID...)
			select {
			case c.replies <- envelope{JSONRPC: "2.0", ID: raw, Error: &rpcError{-32601, "unsupported client method"}}:
			default:
				return failure("limit", "too many server-initiated requests")
			}
		}
		return nil
	}
	if len(msg.ID) == 0 {
		return failure("protocol", "response has no ID")
	}
	c.mu.Lock()
	ch := c.pending[string(msg.ID)]
	delete(c.pending, string(msg.ID))
	c.mu.Unlock()
	if ch != nil {
		if msg.Error != nil {
			ch <- reply{err: failure("server_error", "language server rejected request")}
		} else if len(msg.Result) == 0 {
			ch <- reply{err: failure("protocol", "response has no result")}
		} else {
			ch <- reply{data: append(json.RawMessage(nil), msg.Result...)}
		}
	}
	return nil
}
func (c *connection) send(ctx context.Context, msg envelope) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if len(body) > maxMessageBytes {
		return failure("limit", "LSP message exceeds byte limit")
	}
	data := append([]byte(fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body))), body...)
	select {
	case c.sendGate <- struct{}{}:
		defer func() { <-c.sendGate }()
	case <-ctx.Done():
		return ctx.Err()
	case <-c.ctx.Done():
		return failure("unavailable", "language server exited")
	}
	c.mu.Lock()
	terminal := c.err
	c.mu.Unlock()
	if terminal != nil {
		return terminal
	}
	events := make(chan primitives.PrimitiveEvent, 1)
	c.process.WriteInput(ctx, primitives.ProcessWriteRequest{Source: "lsp", CorrelationID: "write", Data: data}, events)
	select {
	case event := <-events:
		if event.Type != primitives.PrimitiveEventProcessInputWritten {
			c.cancel()
			return failure("unavailable", "language server input failed")
		}
		return nil
	case <-ctx.Done():
		c.cancel()
		return ctx.Err()
	case <-c.done:
		return failure("unavailable", "language server exited")
	}
}
func (c *connection) notify(ctx context.Context, method string, params any) error {
	data, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return c.send(ctx, envelope{JSONRPC: "2.0", Method: method, Params: data})
}
func (c *connection) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	data, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	id := strconv.FormatUint(c.next.Add(1), 10)
	ch := make(chan reply, 1)
	c.mu.Lock()
	if c.err != nil {
		err = c.err
		c.mu.Unlock()
		return nil, err
	}
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, id); c.mu.Unlock() }()
	if err = c.send(ctx, envelope{JSONRPC: "2.0", ID: json.RawMessage(id), Method: method, Params: data}); err != nil {
		return nil, err
	}
	select {
	case value := <-ch:
		return value.data, value.err
	case <-ctx.Done():
		cancelCtx, cancel := context.WithTimeout(c.ctx, 250*time.Millisecond)
		defer cancel()
		_ = c.notify(cancelCtx, "$/cancelRequest", map[string]any{"id": json.RawMessage(id)})
		return nil, ctx.Err()
	case <-c.done:
		return nil, failure("unavailable", "language server exited")
	}
}
func (c *connection) live() bool {
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}

func (c *connection) replyLoop() {
	for {
		select {
		case msg := <-c.replies:
			ctx, cancel := context.WithTimeout(c.ctx, time.Second)
			_ = c.send(ctx, msg)
			cancel()
		case <-c.ctx.Done():
			return
		}
	}
}

func (c *connection) awaitDiagnostics(ctx context.Context, uri string, version int) (diagnostics, error) {
	for {
		c.mu.Lock()
		value, ok := c.diagnostics[uri]
		c.mu.Unlock()
		if ok && value.Version != nil && *value.Version == version {
			return value, nil
		}
		select {
		case <-c.diagnosticsChanged:
		case <-ctx.Done():
			return diagnostics{}, ctx.Err()
		case <-c.done:
			return diagnostics{}, failure("unavailable", "server exited before diagnostics")
		}
	}
}

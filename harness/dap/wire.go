package dap

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"uuid"

	"github.com/unreallabsai/unreal-agent/harness/primitives"
)

type message struct {
	Seq        int            `json:"seq"`
	Type       string         `json:"type"`
	Command    string         `json:"command,omitzero"`
	Arguments  jsontext.Value `json:"arguments,omitzero"`
	RequestSeq int            `json:"request_seq,omitzero"`
	Success    bool           `json:"success"`
	Event      string         `json:"event,omitzero"`
	Body       jsontext.Value `json:"body,omitzero"`
	Message    string         `json:"message,omitzero"`
}
type response struct {
	body jsontext.Value
	err  error
}
type liveSession struct {
	handle            Handle
	attached          bool
	owner             context.Context
	process           *primitives.ProcessInvocation
	cancel            context.CancelFunc
	done              chan struct{}
	initialized       chan struct{}
	mu                sync.Mutex
	writeMu           sync.Mutex
	commandMu         sync.Mutex
	next              int
	pending           map[int]chan response
	state             string
	epoch             uint64
	configurationDone bool
	initializeOnce    sync.Once
}

func (s *liveSession) summary(command string) Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Result{Version: 1, Handle: s.handle, Command: command, Status: s.state, StopEpoch: s.epoch}
}
func (s *liveSession) request(ctx context.Context, command string, args any) (jsontext.Value, error) {
	raw, err := json.Marshal(args)
	if err != nil {
		return nil, &Error{Code: "invalid_request"}
	}
	pending, err := s.send(ctx, command, raw)
	if err != nil {
		return nil, err
	}
	select {
	case result := <-pending:
		return result.body, result.err
	case <-ctx.Done():
		s.interrupt()
		return nil, &Error{Code: "interrupted"}
	case <-s.done:
		return nil, &Error{Code: "expired"}
	}
}

// Ordinary request cancellation expires an uncertain adapter. During owner
// cancellation or Close, keep it alive until the bounded disconnect attempt completes.
func (s *liveSession) interrupt() {
	if s.owner == nil || s.owner.Err() == nil {
		s.cancel()
	}
}
func (s *liveSession) send(ctx context.Context, command string, args jsontext.Value) (<-chan response, error) {
	s.mu.Lock()
	if s.state == "expired" || s.state == "terminated" {
		s.mu.Unlock()
		return nil, &Error{Code: "expired"}
	}
	s.next++
	seq := s.next
	pending := make(chan response, 1)
	s.pending[seq] = pending
	s.mu.Unlock()
	raw, err := json.Marshal(message{Seq: seq, Type: "request", Command: command, Arguments: args})
	if err != nil || len(raw) > MaxFrameBytes {
		return nil, &Error{Code: "request_too_large"}
	}
	wire := append([]byte(fmt.Sprintf("Content-Length: %d\r\n\r\n", len(raw))), raw...)
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	events := make(chan primitives.PrimitiveEvent, 1)
	s.process.WriteInput(ctx, primitives.ProcessWriteRequest{Source: "dap", CorrelationID: primitives.CorrelationID(uuid.New().String()), Data: wire}, events)
	select {
	case event := <-events:
		if event.Type != primitives.PrimitiveEventProcessInputWritten {
			if ctx.Err() != nil {
				s.interrupt()
			} else {
				s.cancel()
			}
			return nil, &Error{Code: "write_failed"}
		}
	case <-ctx.Done():
		s.interrupt()
		return nil, &Error{Code: "interrupted"}
	case <-s.done:
		return nil, &Error{Code: "expired"}
	}
	return pending, nil
}
func (s *liveSession) read(events <-chan primitives.PrimitiveEvent) {
	defer close(s.done)
	defer s.cancel()
	buffer := []byte{}
	invalid := false
	for event := range events {
		switch event.Type {
		case primitives.PrimitiveEventProcessOutput:
			if invalid {
				continue
			}
			output := event.Result.(primitives.ProcessOutputResult)
			if output.Stream != primitives.ProcessStdout {
				continue
			}
			buffer = append(buffer, output.Data...)
			for len(buffer) > 0 {
				body, remaining, complete, err := frame(buffer)
				if err != nil {
					s.fail("protocol_error")
					s.cancel()
					invalid = true
					buffer = nil
					break
				}
				if !complete {
					break
				}
				buffer = remaining
				if err := s.receive(body); err != nil {
					s.fail("protocol_error")
					s.cancel()
					invalid = true
					buffer = nil
					break
				}
			}
		case primitives.PrimitiveEventProcessExited, primitives.PrimitiveEventFailed, primitives.PrimitiveEventCanceled:
			s.fail("expired")
			return
		case primitives.PrimitiveEventProcessStreamFailed:
			s.fail("stream_failed")
			s.cancel()
			invalid = true
		}
	}
	s.fail("expired")
}
func frame(buffer []byte) (body, remaining []byte, complete bool, err error) {
	end := bytes.Index(buffer, []byte("\r\n\r\n"))
	if end < 0 {
		if len(buffer) > 1024 {
			return nil, nil, false, &Error{Code: "invalid_header"}
		}
		return nil, buffer, false, nil
	}
	if end > 1024 {
		return nil, nil, false, &Error{Code: "invalid_header"}
	}
	header := string(buffer[:end])
	if !strings.HasPrefix(header, "Content-Length: ") || strings.Contains(header, "\r\n") {
		return nil, nil, false, &Error{Code: "invalid_header"}
	}
	count, err := strconv.Atoi(strings.TrimPrefix(header, "Content-Length: "))
	if err != nil || count <= 0 || count > MaxFrameBytes {
		return nil, nil, false, &Error{Code: "invalid_length"}
	}
	if len(buffer) < end+4+count {
		return nil, buffer, false, nil
	}
	return buffer[end+4 : end+4+count], buffer[end+4+count:], true, nil
}
func (s *liveSession) receive(raw []byte) error {
	var msg message
	if json.Unmarshal(raw, &msg) != nil || msg.Seq <= 0 {
		return &Error{Code: "invalid_message"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch msg.Type {
	case "response":
		pending, ok := s.pending[msg.RequestSeq]
		if !ok {
			return nil
		}
		delete(s.pending, msg.RequestSeq)
		result := response{body: append(jsontext.Value(nil), msg.Body...)}
		if !msg.Success {
			result.err = &Error{Code: "adapter_rejected"}
		}
		pending <- result
	case "event":
		switch msg.Event {
		case "initialized":
			s.initializeOnce.Do(func() { close(s.initialized) })
		case "stopped":
			s.state = "stopped"
			s.epoch++
		case "continued":
			s.state = "running"
			s.epoch++
		case "terminated", "exited":
			s.state = "terminated"
		}
	case "request":
		return &Error{Code: "unsupported_reverse_request"}
	default:
		return &Error{Code: "invalid_message_type"}
	}
	return nil
}
func (s *liveSession) fail(code string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != "terminated" {
		s.state = "expired"
	}
	for id, ch := range s.pending {
		ch <- response{err: &Error{Code: code}}
		delete(s.pending, id)
	}
}
func (s *liveSession) shutdown() {
	s.commandMu.Lock()
	defer s.commandMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	// Never terminate an unrelated attach target.
	_, _ = s.request(ctx, "disconnect", map[string]any{"restart": false, "terminateDebuggee": !s.attached})
	s.cancel()
	<-s.done
}

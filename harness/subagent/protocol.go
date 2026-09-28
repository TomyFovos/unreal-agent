package subagent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json/v2"
	"errors"
	"fmt"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"io"
	"sync"
)

const MaxFrame = 256 * 1024

type frame struct {
	Version    uint32
	Connection string
	Sender     string
	Target     string
	Kind       string
	ID         inbox.ID                   `json:",omitzero"`
	Config     *ChildConfig               `json:",omitzero"`
	Input      *inbox.Input               `json:",omitzero"`
	Receipt    *host.Receipt              `json:",omitzero"`
	Finish     *sessionstore.FinishRecord `json:",omitzero"`
	Error      string                     `json:",omitzero"`
}

func encodeFrame(f frame) ([]byte, error) {
	data, err := json.Marshal(f)
	if err != nil {
		return nil, err
	}
	if len(data) > MaxFrame {
		return nil, fmt.Errorf("subagent frame too large")
	}
	out := make([]byte, len(data)+4)
	binary.BigEndian.PutUint32(out, uint32(len(data)))
	copy(out[4:], data)
	return out, nil
}
func readFrame(r io.Reader) (frame, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return frame{}, err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 || size > MaxFrame {
		return frame{}, fmt.Errorf("invalid subagent frame length")
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(r, data); err != nil {
		return frame{}, err
	}
	var f frame
	if err := json.Unmarshal(data, &f, json.RejectUnknownMembers(true)); err != nil {
		return f, fmt.Errorf("invalid subagent frame JSON")
	}
	if f.Version != 1 || f.Connection == "" || f.Sender == "" || f.Target == "" || f.Sender == f.Target {
		return f, fmt.Errorf("invalid subagent frame identity")
	}
	return f, nil
}

type frameDecoder struct{ data []byte }

func (d *frameDecoder) push(chunk []byte) ([]frame, error) {
	// Process chunks are bounded independently. Retain at most one incomplete frame.
	d.data = append(d.data, chunk...)
	var frames []frame
	for len(d.data) >= 4 {
		size := int(binary.BigEndian.Uint32(d.data))
		if size == 0 || size > MaxFrame {
			return nil, fmt.Errorf("invalid frame length")
		}
		if len(d.data) < size+4 {
			break
		}
		f, err := readFrame(bytes.NewReader(d.data[:size+4]))
		if err != nil {
			return nil, err
		}
		frames = append(frames, f)
		d.data = d.data[size+4:]
	}
	return frames, nil
}

type reply struct {
	receipt host.Receipt
	err     error
}
type endpoint struct {
	ctx                       context.Context
	connection, local, remote string
	write                     func(context.Context, frame) error
	receive                   func(context.Context, inbox.Input) (host.Receipt, error)
	mu                        sync.Mutex
	pending                   map[inbox.ID]chan reply
	sent                      map[inbox.ID][32]byte
}

func (e *endpoint) send(ctx context.Context, input inbox.Input) (host.Receipt, error) {
	if err := input.Validate(); err != nil {
		return host.Receipt{}, err
	}
	payload := input.Payload.Clone()
	if len(payload) > 0 {
		if err := payload.Canonicalize(); err != nil {
			return host.Receipt{}, err
		}
	}
	digest := sha256.Sum256(append([]byte(string(input.Kind)+"\\x00"), payload...))
	ch := make(chan reply, 1)
	e.mu.Lock()
	if e.sent == nil {
		e.sent = map[inbox.ID][32]byte{}
	}
	if previous, ok := e.sent[input.ID]; ok && previous != digest {
		e.mu.Unlock()
		return host.Receipt{}, host.ErrConflict
	}
	e.sent[input.ID] = digest
	if e.pending == nil {
		e.pending = map[inbox.ID]chan reply{}
	}
	if _, ok := e.pending[input.ID]; ok {
		e.mu.Unlock()
		return host.Receipt{}, fmt.Errorf("input already in flight")
	}
	if len(e.pending) >= 32 {
		e.mu.Unlock()
		return host.Receipt{}, fmt.Errorf("peer backpressure limit")
	}
	e.pending[input.ID] = ch
	e.mu.Unlock()
	defer func() { e.mu.Lock(); delete(e.pending, input.ID); e.mu.Unlock() }()
	f := e.envelope("input")
	f.ID = input.ID
	f.Input = &input
	if err := e.write(ctx, f); err != nil {
		return host.Receipt{}, err
	}
	select {
	case r := <-ch:
		return r.receipt, r.err
	case <-ctx.Done():
		return host.Receipt{}, context.Cause(ctx)
	case <-e.ctx.Done():
		return host.Receipt{}, context.Cause(e.ctx)
	}
}
func (e *endpoint) envelope(kind string) frame {
	return frame{Version: 1, Connection: e.connection, Sender: e.local, Target: e.remote, Kind: kind}
}
func (e *endpoint) accept(f frame) error {
	if f.Connection != e.connection || f.Sender != e.remote || f.Target != e.local {
		return fmt.Errorf("stale or mismatched peer channel")
	}
	switch f.Kind {
	case "ack":
		if f.ID == "" || (f.Receipt == nil && f.Error == "") {
			return fmt.Errorf("invalid peer acknowledgement")
		}
		e.mu.Lock()
		ch := e.pending[f.ID]
		e.mu.Unlock()
		if ch != nil {
			r := reply{}
			if f.Receipt != nil {
				if f.Receipt.ID != f.ID {
					return fmt.Errorf("receipt identity mismatch")
				}
				r.receipt = *f.Receipt
			}
			if f.Error != "" {
				if f.Error == "conflict" {
					r.err = host.ErrConflict
				} else {
					r.err = errors.New("peer rejected input")
				}
			}
			select {
			case ch <- r:
			default:
			}
		}
		return nil
	case "input":
		if f.Input == nil || f.ID != f.Input.ID {
			return fmt.Errorf("invalid peer input frame")
		}
		r, err := e.receive(e.ctx, *f.Input)
		ack := e.envelope("ack")
		ack.ID = f.ID
		if err != nil {
			ack.Error = "rejected"
			if errors.Is(err, host.ErrConflict) {
				ack.Error = "conflict"
			}
		} else {
			ack.Receipt = &r
		}
		return e.write(e.ctx, ack)
	default:
		return fmt.Errorf("unexpected peer frame")
	}
}

package subagent

import (
	"bytes"
	"context"
	"errors"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"testing"
)

func TestFramesPartialCombinedAndBounds(t *testing.T) {
	f := frame{Version: 1, Connection: "c", Sender: "p", Target: "s", Kind: "welcome"}
	data, err := encodeFrame(f)
	if err != nil {
		t.Fatal(err)
	}
	var d frameDecoder
	for _, b := range data[:len(data)-1] {
		frames, err := d.push([]byte{b})
		if err != nil || len(frames) != 0 {
			t.Fatal(frames, err)
		}
	}
	frames, err := d.push(append(data[len(data)-1:], data...))
	if err != nil || len(frames) != 2 {
		t.Fatal(frames, err)
	}
	if _, err = readFrame(bytes.NewReader([]byte{255, 255, 255, 255})); err == nil {
		t.Fatal("oversized frame")
	}
	d = frameDecoder{}
	if _, err = d.push([]byte{0, 0, 0, 0}); err == nil {
		t.Fatal("zero length")
	}
}
func TestChannelBindingAndAckAfterCommit(t *testing.T) {
	committed := false
	ack := false
	ep := endpoint{ctx: context.Background(), connection: "c", local: "parent", remote: "child"}
	ep.receive = func(context.Context, inbox.Input) (host.Receipt, error) {
		committed = true
		return host.Receipt{ID: "one", Sequence: 7}, nil
	}
	ep.write = func(f frame) error {
		if !committed {
			t.Fatal("ack before commit")
		}
		ack = true
		return nil
	}
	f := frame{Version: 1, Connection: "old", Sender: "child", Target: "parent", Kind: "input", ID: "one", Input: &inbox.Input{ID: "one"}}
	if err := ep.accept(f); err == nil || committed {
		t.Fatal("stale connection")
	}
	f.Connection = "c"
	if err := ep.accept(f); err != nil || !ack {
		t.Fatal(err)
	}
	ep.receive = func(context.Context, inbox.Input) (host.Receipt, error) { return host.Receipt{}, host.ErrConflict }
	ep.write = func(f frame) error {
		if f.Error != "conflict" {
			t.Fatal(f)
		}
		return nil
	}
	if err := ep.accept(f); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(host.ErrConflict, host.ErrConflict) {
		t.Fatal("unreachable")
	}
}

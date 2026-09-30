package subagent

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"errors"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/projectinstructions"
	"strings"
	"testing"
	"time"
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
	ep.write = func(_ context.Context, f frame) error {
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
	ep.write = func(_ context.Context, f frame) error {
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

func TestCancellationBeforeHandlerRegistration(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	config := Template{Workspace: dir, Runtime: jsontext.Value("{}"), Policy: permission.Config{Tools: []string{"Finish"}}}
	m, err := NewManager(ctx, Config{Owner: &host.Session{ID: "parent"}, Directory: dir, Binary: "/missing", Templates: map[string]Template{"default": config}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	spec, _ := NewSpec(Plan{Version: 1, Action: "start", ParentID: "parent", Template: "default", Configuration: &config, Text: "task"})
	op := operation.Operation{ID: "op", Type: spec.Type, Version: spec.Version, State: spec.State, MaxOutputLength: spec.MaxOutputLength, Status: operation.StatusReady}
	if err = m.CancelRemoteJob(op.ID, "cancel before add"); err != nil {
		t.Fatal(err)
	}
	if err = m.AddRemoteJob(op); err != nil {
		t.Fatal(err)
	}
	select {
	case update := <-m.RemoteJobUpdates():
		if update.Status != operation.StatusCanceled {
			t.Fatal(update)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel lost before registration")
	}
}
func TestBlockedProcessWriteIsCanceledAndDrained(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	child := ChildConfig{ParentID: "parent", ChildID: "child", Workspace: t.TempDir()}
	p := startProcess(ctx, Config{Owner: &host.Session{ID: "parent"}, Binary: "/bin/sh", Arguments: []string{"-c", "sleep 60"}}, child)
	defer func() { p.cancel(); <-p.done }()
	writeCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		f := p.endpoint.envelope("input")
		f.Error = strings.Repeat("x", 200000)
		done <- p.write(writeCtx, f)
	}()
	stop()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("blocked writer succeeded")
		}
	case <-ctx.Done():
		t.Fatal("blocked writer leaked")
	}
	select {
	case <-p.done:
	case <-ctx.Done():
		t.Fatal("canceled process leaked")
	}
}

func TestLostAckRetryRejectsChangedPayload(t *testing.T) {
	ep := endpoint{ctx: t.Context(), connection: "one", local: "parent", remote: "child"}
	writes := 0
	ep.write = func(_ context.Context, f frame) error {
		writes++
		if writes == 1 {
			return nil
		} // Receiver committed but the ACK was lost.
		return ep.accept(frame{Version: 1, Connection: "one", Sender: "child", Target: "parent", Kind: "ack", ID: f.ID, Receipt: &host.Receipt{ID: f.ID, Sequence: 42}})
	}
	first := inbox.Input{ID: "one", Kind: inbox.InputExternal, Payload: jsontext.Value(`"original"`)}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := ep.send(canceled, first); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	changed := first
	changed.Payload = jsontext.Value(`"different"`)
	if _, err := ep.send(t.Context(), changed); !errors.Is(err, host.ErrConflict) {
		t.Fatal("changed retry admitted", err)
	}
	receipt, err := ep.send(t.Context(), first)
	if err != nil || receipt.Sequence != 42 || writes != 2 {
		t.Fatal(receipt, err, writes)
	}
}

func TestChildHandshakeRejectsUnknownInstructionVersion(t *testing.T) {
	id := operation.ID("operation")
	c := ChildConfig{Version: 1, ParentID: "parent", ChildID: ChildID("parent", id), OperationID: id, SessionDirectory: t.TempDir(), Workspace: t.TempDir(), Runtime: []byte("{}"), ReadyID: "ready:operation", Task: "delegated", Policy: permission.Config{Tools: []string{"Finish"}}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	// Omitted instructions remain compatible with legacy configurations.
	snapshot := projectinstructions.None()
	snapshot.Version++
	c.ProjectInstructions = &snapshot
	if err := c.Validate(); err == nil {
		t.Fatal("unknown snapshot version accepted")
	}
}

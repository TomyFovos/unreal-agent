package subagent_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	sub "github.com/unreallabsai/unreal-agent/harness/subagent"
)

// The synthetic helper has already committed Finish before Serve writes this
// frame. Record genuine readiness before forwarding it, then hold process exit.
// A timer is deliberately not the evidence that the held process started.
type heldFinishOutput struct {
	enabled    bool
	forgedMode string
	directory  string
}

func (w *heldFinishOutput) Close() error { return os.Stdout.Close() }
func (w *heldFinishOutput) Write(b []byte) (int, error) {
	if w.enabled && len(b) >= 4 && int(binary.BigEndian.Uint32(b[:4])) == len(b)-4 {
		var header struct{ Kind string }
		if json.Unmarshal(b[4:], &header) == nil && header.Kind == "finish" {
			if err := os.WriteFile(filepath.Join(w.directory, "held-finish-ready"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
				return 0, err
			}
		}
	}
	n, err := os.Stdout.Write(b)
	if err == nil && w.forgedMode != "" && len(b) >= 4 {
		var frame map[string]jsontext.Value
		if json.Unmarshal(b[4:], &frame) == nil && string(frame["Kind"]) == `"welcome"` {
			if e := os.WriteFile(filepath.Join(w.directory, "untrusted-finish-ready"), []byte(strconv.Itoa(os.Getpid())), 0600); e != nil {
				return 0, e
			}
			frame["Kind"] = jsontext.Value(`"finish"`)
			if w.forgedMode != "missing-finish" {
				version := uint32(1)
				if w.forgedMode == "invalid-finish" {
					version = 2
				}
				value, _ := json.Marshal(sessionstore.FinishRecord{Version: version, OperationID: "uncommitted", Result: sessionstore.FinishResult{Status: "completed", Summary: "synthetic untrusted finish notice"}})
				frame["Finish"] = value
			}
			payload, e := json.Marshal(frame)
			if e != nil {
				return 0, e
			}
			encoded := make([]byte, len(payload)+4)
			binary.BigEndian.PutUint32(encoded, uint32(len(payload)))
			copy(encoded[4:], payload)
			count := 1
			if w.forgedMode == "forged-finish" {
				count = 64
			}
			for range count {
				if _, e = os.Stdout.Write(encoded); e != nil {
					return 0, e
				}
			}
		}
	}
	return n, err
}
func holdFinishedHelper() {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGUSR1)
	defer signal.Stop(signals)
	<-signals // Only the test-owned parent cancellation should terminate this child.
}

func TestCanonicalFinishProgressDoesNotWaitForProcessExit(t *testing.T) {
	h, parent := newParent(t, "finish-held-exit")
	defer h.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	subscription, err := parent.Subscribe(0, 256, 32)
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Cancel()
	var terminal operation.Operation
	find := func(ops []operation.Operation) bool {
		for _, op := range ops {
			if op.Status == operation.StatusCompleted {
				terminal = op
				return true
			}
		}
		return false
	}
	done := find(subscription.Initial.Operations)
	for !done {
		select {
		case <-ctx.Done():
			v, _ := parent.Inspect(0, 256)
			var ready []byte
			workspace := parentWorkspace(v.Operations)
			e := os.ErrNotExist
			if workspace != "" {
				ready, e = os.ReadFile(filepath.Join(workspace, "held-finish-ready"))
			}
			t.Fatalf("canonical Finish progress stalled: ready=%t held_pid_parseable=%t operation_count=%d", e == nil, len(bytes.TrimSpace(ready)) > 0, len(v.Operations))
		case event, open := <-subscription.Events:
			if !open {
				t.Fatal("parent stopped before child terminal result")
			}
			if event.Operation != nil {
				done = find([]operation.Operation{*event.Operation})
			}
		}
	}
	plan, err := sub.DecodePlan(terminal)
	if err != nil {
		t.Fatal("completed child lost canonical binding")
	}
	ready, err := os.ReadFile(filepath.Join(plan.Configuration.Workspace, "held-finish-ready"))
	pid, parseErr := strconv.Atoi(string(ready))
	if err != nil || parseErr != nil || pid < 1 {
		t.Fatal("child hold readiness was not proven")
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatal("completed child operation preceded owned process reap")
	}
	t.Log("durable Finish observed; held child canceled and reaped before parent Operation completion")
}

func parentWorkspace(ops []operation.Operation) string {
	for _, op := range ops {
		if p, err := sub.DecodePlan(op); err == nil && p.Configuration != nil {
			return p.Configuration.Workspace
		}
	}
	return ""
}

func TestUntrustedFinishNoticeCannotCompleteParentOperation(t *testing.T) {
	for _, mode := range []string{"forged-finish", "missing-finish", "invalid-finish"} {
		t.Run(mode, func(t *testing.T) {
			h, parent := newParent(t, mode)
			defer h.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			subscription, err := parent.Subscribe(0, 256, 32)
			if err != nil {
				t.Fatal(err)
			}
			defer subscription.Cancel()
			view := subscription.Initial
			ready := func() bool {
				if len(view.Operations) != 1 {
					return false
				}
				op := view.Operations[0]
				if op.Status == operation.StatusCompleted {
					t.Fatal("untrusted Finish notification completed parent operation")
				}
				if mode != "forged-finish" {
					return op.Status == operation.StatusFailed
				}
				for _, item := range view.History.Items {
					if in, ok := item.Data.(inbox.Input); ok && in.Kind == inbox.InputPeer {
						peer, e := in.DecodePeerMessage()
						if e == nil && peer.Kind == "ready" {
							return true
						}
					}
				}
				return false
			}
			for !ready() {
				select {
				case <-ctx.Done():
					t.Fatal("untrusted Finish fixture did not reach its intended boundary")
				case _, open := <-subscription.Events:
					if !open {
						t.Fatal("parent stopped")
					}
					view, err = parent.Inspect(0, 256)
					if err != nil {
						t.Fatal("canonical parent inspect failed")
					}
				}
			}
			plan, err := sub.DecodePlan(view.Operations[0])
			if err != nil {
				t.Fatal("child binding missing")
			}
			raw, err := os.ReadFile(filepath.Join(plan.Configuration.Workspace, "untrusted-finish-ready"))
			pid, parseErr := strconv.Atoi(string(raw))
			if err != nil || parseErr != nil || pid < 1 {
				t.Fatal("actual forged/missing/invalid Finish readiness not proven")
			}
			child, err := sub.ReadChild(ctx, plan.Configuration.Workspace, plan.ChildID, 0, 256)
			if err != nil || child.Finish != nil {
				t.Fatal("fixture unexpectedly committed canonical Finish")
			}
			if mode == "forged-finish" {
				if _, err := parent.CancelOperation(ctx, parent.Generation, "cancel-untrusted", view.Operations[0].ID); err != nil {
					t.Fatal("owned child cancellation failed")
				}
				terminal := awaitChild(t, parent, true)
				if terminal.Status != operation.StatusCanceled {
					t.Fatal("unpersisted notification was accepted as completion")
				}
			}
			if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
				t.Fatal("negative fixture child was not reaped")
			}
		})
	}
}

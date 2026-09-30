package dap

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"os"
	"testing"
	"time"
)

func TestReviewCloseDuringAttachedRequestStillDetaches(t *testing.T) {
	for _, ownerCanceled := range []bool{false, true} {
		t.Run(fmt.Sprintf("owner-canceled-%t", ownerCanceled), func(t *testing.T) {
			testCloseDuringAttachedRequest(t, ownerCanceled)
		})
	}
}
func testCloseDuringAttachedRequest(t *testing.T, ownerCanceled bool) {
	m, cleanup := testManager(t, "hang", permission.Unrestricted())
	out := call(t, m, startRequest(m, "attach"))
	request := Request{Version: 1, OwnerGeneration: m.Generation(), Handle: out.Handle, Command: "evaluate", StopEpoch: out.StopEpoch, Expression: "x"}
	done := make(chan error, 1)
	go func() { _, err := m.Execute(context.Background(), request); done <- err }()
	m.mu.Lock()
	session := m.sessions[out.Handle.ID]
	m.mu.Unlock()
	deadline := time.After(2 * time.Second)
	for {
		session.mu.Lock()
		pending := len(session.pending)
		session.mu.Unlock()
		if pending > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("evaluation did not become pending")
		case <-time.After(time.Millisecond):
		}
	}
	if ownerCanceled {
		// Exercise the owner-cancellation callback before explicit Close joins it.
		m.cancel()
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if codeOf(err) != "interrupted" {
			t.Fatalf("active request result: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("active request did not drain after Close")
	}
	data, err := os.ReadFile(cleanup)
	if err != nil {
		t.Fatalf("attached adapter killed before disconnect while request active: %v", err)
	}
	var fields struct {
		Terminate *bool `json:"terminateDebuggee"`
	}
	if err = json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if fields.Terminate == nil || *fields.Terminate {
		t.Fatal("Close did not explicitly preserve the attached debuggee", string(data))
	}
}

package dap

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/permission"
)

func TestStartupCancellationPreservesOwnershipCleanup(t *testing.T) {
	for _, command := range []string{"attach", "launch"} {
		for _, mode := range []string{"pending-start", "pending-start-configured", "pending-start-no-disconnect-response"} {
			for _, ownerCanceled := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/owner-canceled-%t", command, mode, ownerCanceled), func(t *testing.T) {
					owner, cancelOwner := context.WithCancel(t.Context())
					defer cancelOwner()
					m, cleanup := testManagerWithContext(t, owner, mode, permission.Unrestricted())
					started := make(chan error, 1)
					go func() { _, err := m.Execute(context.Background(), startRequest(m, command)); started <- err }()
					deadline := time.After(3 * time.Second)
					for {
						data, err := os.ReadFile(cleanup + ".started")
						if err == nil && string(data) == command {
							break
						}
						select {
						case <-deadline:
							t.Fatal("adapter did not receive startup request")
						case <-time.After(time.Millisecond):
						}
					}
					if ownerCanceled {
						cancelOwner()
					}
					closed := make(chan error, 1)
					go func() { closed <- m.Close() }()
					select {
					case err := <-closed:
						if err != nil {
							t.Fatal(err)
						}
					case <-time.After(3 * time.Second):
						t.Fatal("startup shutdown exceeded its bound")
					}
					select {
					case err := <-started:
						if codeOf(err) != "interrupted" {
							t.Fatalf("startup outcome: %v", err)
						}
					case <-time.After(time.Second):
						t.Fatal("startup did not drain")
					}
					data, err := os.ReadFile(cleanup)
					if err != nil {
						t.Fatalf("adapter stopped before receiving disconnect: %v", err)
					}
					var fields struct {
						Terminate *bool `json:"terminateDebuggee"`
					}
					if err := json.Unmarshal(data, &fields); err != nil {
						t.Fatal(err)
					}
					if fields.Terminate == nil || *fields.Terminate != (command == "launch") {
						t.Fatalf("wrong startup ownership cleanup: %s", data)
					}
					m.mu.Lock()
					live := m.sessions["debug"]
					m.mu.Unlock()
					select {
					case <-live.done:
					default:
						t.Fatal("adapter process not drained")
					}
				})
			}
		}
	}
}

func TestCallerCanceledStartupStillCleansAdapter(t *testing.T) {
	m, cleanup := testManager(t, "pending-start", permission.Unrestricted())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := m.Execute(ctx, startRequest(m, "attach")); result <- err }()
	deadline := time.After(3 * time.Second)
	for {
		if _, err := os.Stat(cleanup + ".started"); err == nil {
			break
		}
		select {
		case <-deadline:
			t.Fatal("adapter did not receive attach")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-result:
		if codeOf(err) != "interrupted" {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("caller cancellation did not drain")
	}
	m.mu.Lock()
	live := m.sessions["debug"]
	m.mu.Unlock()
	select {
	case <-live.done:
	case <-time.After(3 * time.Second):
		t.Fatal("ordinary startup failure leaked adapter")
	}
	if _, err := os.Stat(cleanup); !os.IsNotExist(err) {
		t.Fatalf("ordinary startup cancellation unexpectedly disconnected: %v", err)
	}
}

func TestRejectedStartupStillCleansAdapter(t *testing.T) {
	for _, command := range []string{"attach", "launch"} {
		t.Run(command, func(t *testing.T) {
			m, cleanup := testManager(t, "reject-start", permission.Unrestricted())
			if _, err := m.Execute(t.Context(), startRequest(m, command)); codeOf(err) != "adapter_rejected" {
				t.Fatalf("startup rejection: %v", err)
			}
			m.mu.Lock()
			live := m.sessions["debug"]
			m.mu.Unlock()
			select {
			case <-live.done:
			case <-time.After(3 * time.Second):
				t.Fatal("startup rejection leaked adapter")
			}
			if _, err := os.Stat(cleanup); !os.IsNotExist(err) {
				t.Fatalf("ordinary startup rejection unexpectedly disconnected: %v", err)
			}
		})
	}
}

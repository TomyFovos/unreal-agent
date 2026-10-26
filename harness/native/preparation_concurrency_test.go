package native

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/mutation"
	"github.com/unreallabsai/unreal-agent/harness/operation"
)

// This exercises the real handler, Service locks/approved descriptor and the
// body-read boundary. The synthetic owner ACK below is not a disk-durability
// test; Coordinator/Host tests separately verify that checkpoint.
func TestNativeBlockedLargeBodyObservationAllowsReadAndCancel(t *testing.T) {
	for _, size := range []int{64 << 10, mutation.MaxFileBytes} {
		t.Run(fmt.Sprintf("bytes_%d", size), func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "large")
			before := bytes.Repeat([]byte{'x'}, size)
			if err := os.WriteFile(path, before, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "independent"), []byte("synthetic independent read"), 0600); err != nil {
				t.Fatal(err)
			}
			started, release := make(chan struct{}), make(chan struct{})
			var startOnce, releaseOnce, contextOnce sync.Once
			t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
			var workerContext context.Context
			var bodyHashes int
			service, err := mutation.New(mutation.Config{Root: root, StateDir: filepath.Join(t.TempDir(), "mutation"), Authorize: func(ctx context.Context, target string, _ bool) error {
				if target == path {
					contextOnce.Do(func() { workerContext = ctx })
				}
				return nil
			}, ObserveIO: func(target, phase string) {
				if target != path {
					return
				}
				if phase == "read" {
					startOnce.Do(func() { close(started) })
					<-release
				}
				if phase == "hash" {
					bodyHashes++
				}
			}})
			if err != nil {
				t.Fatal(err)
			}
			handler, err := NewHandler(t.Context(), Executor{Files: service}, "synthetic-blocked-body")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { releaseOnce.Do(func() { close(release) }); handler.Close() })
			makeOperation := func(id string, request Request) operation.Operation {
				spec, err := Spec(request)
				if err != nil {
					t.Fatal(err)
				}
				return operation.Operation{ID: operation.ID(id), ToolName: request.Action, Type: spec.Type, Version: spec.Version, MaxOutputLength: spec.MaxOutputLength, State: spec.State, Status: operation.StatusReady}
			}
			expected := mutation.RevisionOf(before)
			large := operation.WithPreparationRequired(makeOperation("synthetic-large", Request{Version: Version, Action: "write", Path: "large", Expected: &expected, Content: "must-not-write"}), true)
			if err := handler.AddRemoteJob(large); err != nil {
				t.Fatal(err)
			}
			select {
			case preparation := <-handler.RemoteJobPreparations():
				preparation.Result <- operation.PreparationResult{Operation: operation.WithExecutionBasis(large, preparation.Basis)}
			case <-time.After(time.Second):
				t.Fatal("authorized worker did not reach preparation checkpoint")
			}
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("large target never reached its body-read boundary")
			}
			independent := makeOperation("synthetic-independent", Request{Version: Version, Action: "read", Path: "independent"})
			added := make(chan error, 1)
			go func() { added <- handler.AddRemoteJob(independent) }()
			select {
			case err := <-added:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("blocked body I/O held the handler registration mutex")
			}
			select {
			case update := <-handler.RemoteJobUpdates():
				state, err := operation.DecodeRemoteJobState(update)
				var result Result
				if err != nil || update.ID != independent.ID || update.Status != operation.StatusCompleted || json.Unmarshal(state.Handle, &result) != nil || result.Content != "synthetic independent read" {
					t.Fatal("actual independent Read did not finish while large I/O was blocked", err)
				}
			case <-time.After(time.Second):
				t.Fatal("blocked body I/O stopped independent Read execution")
			}
			canceled := make(chan error, 1)
			go func() { canceled <- handler.CancelRemoteJob(large.ID, "synthetic cancellation during blocked read") }()
			select {
			case err := <-canceled:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("blocked body I/O held handler cancellation")
			}
			if workerContext == nil || workerContext.Err() == nil {
				t.Fatal("CancelRemoteJob did not revoke the worker during blocked I/O")
			}
			releaseOnce.Do(func() { close(release) })
			select {
			case update := <-handler.RemoteJobUpdates():
				if update.ID != large.ID || update.Status != operation.StatusCanceled {
					t.Fatal("canceled large operation did not terminate with typed cancellation", update.Status)
				}
			case <-time.After(time.Second):
				t.Fatal("canceled large operation did not terminate")
			}
			if bodyHashes != 0 {
				t.Fatal("large body was hashed after cancellation at its read boundary")
			}
			if content, err := os.ReadFile(path); err != nil || !bytes.Equal(content, before) {
				t.Fatal("canceled large operation changed bytes")
			}
			t.Logf("P08 actual native handler payload_bytes=%d blocked_body_boundary=true independent_Read_completed=true cancel_delivered_before_release=true body_hashes_after_cancel=0 mutation_effects=0", size)
		})
	}
}

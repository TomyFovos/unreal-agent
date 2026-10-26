package native

import (
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/mutation"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
)

func TestNativePreparationFailureHasTypedUnexecutedMutationOutcome(t *testing.T) {
	for _, action := range []string{"write", "edit"} {
		for _, fault := range []string{"denied", "revoked", "ack_failed"} {
			t.Run(action+"_"+fault, func(t *testing.T) {
				root := t.TempDir()
				path := filepath.Join(root, "b")
				var authorizations, bodyIO int
				started := make(chan struct{}, 1)
				service, err := mutation.New(mutation.Config{Root: root, StateDir: filepath.Join(t.TempDir(), "mutation"), ObserveIO: func(_ string, phase string) {
					if phase == "read" || phase == "hash" {
						bodyIO++
					}
				}, Authorize: func(ctx context.Context, _ string, _ bool) error {
					authorizations++
					switch fault {
					case "denied":
						return &permission.Error{Code: permission.Denied, Capability: "filesystem", Reason: "synthetic current authority denied"}
					case "revoked":
						started <- struct{}{}
						<-ctx.Done()
						return ctx.Err()
					}
					return nil
				}})
				if err != nil {
					t.Fatal(err)
				}
				handler, err := NewHandler(t.Context(), Executor{Files: service}, "synthetic-preparation-failure")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { handler.Close() })
				r := Request{Version: Version, Action: action, Path: "b", Expected: &mutation.Revision{}, Content: "must-not-write"}
				if action == "edit" {
					r.Content, r.OldText, r.NewText = "", "before", "after"
				}
				spec, err := Spec(r)
				if err != nil {
					t.Fatal(err)
				}
				current := operation.WithPreparationRequired(operation.Operation{ID: "synthetic-new-b", ToolName: action, Type: spec.Type, Version: spec.Version, State: spec.State, MaxOutputLength: spec.MaxOutputLength, Status: operation.StatusReady}, true)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if err := handler.AddRemoteJobWithContext(ctx, current); err != nil {
					t.Fatal(err)
				}
				if fault == "revoked" {
					select {
					case <-started:
						cancel()
					case <-time.After(time.Second):
						t.Fatal("worker did not reach concrete authorization")
					}
				}
				if fault == "ack_failed" {
					select {
					case prepared := <-handler.RemoteJobPreparations():
						if prepared.ID != current.ID || len(prepared.Basis) == 0 {
							t.Fatal("authorized metadata request missing")
						}
						if _, err := os.Stat(path); !os.IsNotExist(err) || bodyIO != 0 {
							t.Fatal("preparation channel delivery started the executor")
						}
						prepared.Result <- operation.PreparationResult{Operation: operation.WithExecutionBasis(current, prepared.Basis), Err: operation.ErrPreparationUncommitted}
					case <-time.After(time.Second):
						t.Fatal("authorized worker did not await durable ACK")
					}
				}
				var update operation.Operation
				select {
				case update = <-handler.RemoteJobUpdates():
				case <-time.After(time.Second):
					t.Fatal("pre-effect failure had no terminal result")
				}
				state, err := operation.DecodeRemoteJobState(update)
				var result Result
				if err != nil || json.Unmarshal(state.Handle, &result) != nil || result.Mutation == nil || result.Path != "b" || result.Code != string(result.Mutation.Code) || len(result.Mutation.Targets) != 1 || result.Mutation.Targets[0].Path != "b" || result.Mutation.Targets[0].Code != mutation.NotApplied || result.Mutation.Targets[0].Revision != (mutation.Revision{}) {
					t.Fatal("missing exact typed not-applied mutation evidence", err)
				}
				if fault == "revoked" {
					if update.Status != operation.StatusCanceled || result.Mutation.Code != mutation.Canceled {
						t.Fatal("revoked preparation was not typed canceled")
					}
				} else if update.Status != operation.StatusFailed || result.Mutation.Code != mutation.Denied || result.Denial == nil {
					t.Fatal("failed preparation was not typed denied")
				}
				if authorizations == 0 || bodyIO != 0 {
					t.Fatal("refused preparation read or hashed a target body", authorizations, bodyIO)
				}
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("refused preparation executed a mutation")
				}
			})
		}
	}
}

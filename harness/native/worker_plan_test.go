package native

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/mutation"
	"github.com/unreallabsai/unreal-agent/harness/operation"
)

func TestNativePreparedWorkerUsesUnchangedStrictPlan(t *testing.T) {
	h, ctx, root := recoveryBasisFixture(t)
	request := Request{Version: Version, Action: "write", Path: "worker-plan", Expected: &mutation.Revision{}, Content: strings.Repeat("<>&日本語\n", 4096)}
	spec, err := Spec(request)
	if err != nil {
		t.Fatal(err)
	}
	current := operation.WithPreparationRequired(operation.Operation{ID: "synthetic-worker-plan", ToolName: request.Action, Status: operation.StatusReady, Type: spec.Type, Version: spec.Version, State: spec.State, MaxOutputLength: spec.MaxOutputLength}, true)
	before := current.State.Clone()
	type reply struct {
		current operation.Operation
		ctx     context.Context
		err     error
	}
	done := make(chan reply, 1)
	go func() {
		op, executionContext, err := h.prepareWorker(ctx, current)
		done <- reply{op, executionContext, err}
	}()
	var prepared operation.PreparationRequest
	select {
	case prepared = <-h.RemoteJobPreparations():
	case <-time.After(time.Second):
		t.Fatal("authorized worker did not request its owner checkpoint")
	}
	if prepared.ID != current.ID || len(prepared.Basis) == 0 || !bytes.Equal(current.State, before) {
		t.Fatal("preparation changed accepted plan or omitted its basis")
	}
	if _, err := os.Stat(filepath.Join(root, request.Path)); !os.IsNotExist(err) {
		t.Fatal("preparation performed the mutation before the owner ACK")
	}
	select {
	case <-done:
		t.Fatal("worker returned before its owner acknowledgement")
	default:
	}
	var basis executionBasis
	if err := json.Unmarshal(prepared.Basis, &basis); err != nil {
		t.Fatal(err)
	}
	state, err := operation.DecodeRemoteJobState(current)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(state.Plan.Data)
	if basis.PlanSHA256 != hex.EncodeToString(digest[:]) || basis.Action != request.Action || len(basis.Footprint.Targets) != 1 || basis.Footprint.Targets[0].Path != filepath.Join(root, request.Path) {
		t.Fatal("basis is not bound to the original exact accepted plan/target")
	}
	acknowledged := operation.WithExecutionBasis(current, prepared.Basis)
	prepared.Result <- operation.PreparationResult{Operation: acknowledged}
	var result reply
	select {
	case result = <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not return after its owner acknowledgement")
	}
	if result.err != nil || !bytes.Equal(result.current.State, before) || !operation.SameExecutionMetadata(result.current, acknowledged) {
		t.Fatal("worker lost the accepted plan or owner checkpoint", result.err)
	}
	// This is a synthetic ACK fixture, not a durability test. Host/Coordinator
	// tests verify disk persistence. The real Mutation must use these parameters.
	applied := h.executor.Execute(result.ctx, "synthetic-worker-plan-execute", request)
	if applied.Code != "applied" || applied.Mutation == nil || applied.Mutation.Code != mutation.Applied {
		t.Fatal("unchanged granted worker request did not reach Mutation", applied.Code)
	}
	got, err := os.ReadFile(filepath.Join(root, request.Path))
	if err != nil || string(got) != request.Content {
		t.Fatal("worker execution parameters changed", err)
	}
}

func TestNativePreparedWorkerRejectsStrictInvalidPlan(t *testing.T) {
	for _, fault := range []string{"unknown-member", "wrong-tool", "invalid-request"} {
		t.Run(fault, func(t *testing.T) {
			h, ctx, root := recoveryBasisFixture(t)
			current := operation.WithPreparationRequired(basisWrite(t, "synthetic-worker-invalid", "must-not-exist"), true)
			switch fault {
			case "wrong-tool":
				current.ToolName = "edit"
			default:
				state, err := operation.DecodeRemoteJobState(current)
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]any
				if err := json.Unmarshal(state.Plan.Data, &fields); err != nil {
					t.Fatal(err)
				}
				if fault == "unknown-member" {
					fields["grant"] = true
				} else {
					fields["version"] = 0
				}
				state.Plan.Data, err = json.Marshal(fields)
				if err != nil {
					t.Fatal(err)
				}
				current.State, err = json.Marshal(state)
				if err != nil {
					t.Fatal(err)
				}
			}
			before := operation.CloneExecutionMetadata(current)
			before.State = before.State.Clone()
			got, _, err := h.prepareWorker(ctx, current)
			if !errors.Is(err, operation.ErrIndependenceUnproven) || !reflect.DeepEqual(got, before) {
				t.Fatal("strict invalid preparation was accepted or changed its checkpoint", err)
			}
			if _, err := h.PrepareExecution(ctx, current); !errors.Is(err, operation.ErrIndependenceUnproven) {
				t.Fatal("public preparation wrapper lost strict validation", err)
			}
			valid, err := h.PrepareExecution(ctx, basisWrite(t, "synthetic-valid-basis", "must-not-exist"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := h.recoveryExecutionContext(ctx, operation.WithExecutionBasis(current, valid.ExecutionBasis())); !errors.Is(err, operation.ErrIndependenceUnproven) {
				t.Fatal("standalone context wrapper lost strict validation", err)
			}
			select {
			case <-h.RemoteJobPreparations():
				t.Fatal("invalid plan requested an owner acknowledgement")
			default:
			}
			if _, err := os.Stat(filepath.Join(root, "must-not-exist")); !os.IsNotExist(err) {
				t.Fatal("invalid plan reached Mutation")
			}
		})
	}
}

func TestNativeUnpreparedWorkerKeepsOrdinaryReadPath(t *testing.T) {
	h, ctx, _ := recoveryBasisFixture(t)
	spec, err := Spec(Request{Version: Version, Action: "read", Path: "ordinary-read"})
	if err != nil {
		t.Fatal(err)
	}
	current := operation.Operation{ID: "synthetic-ordinary-read", ToolName: "read", Type: spec.Type, Version: spec.Version, State: spec.State, MaxOutputLength: spec.MaxOutputLength, Status: operation.StatusReady}
	got, executionContext, err := h.prepareWorker(ctx, current)
	if err != nil || !reflect.DeepEqual(got, current) || executionContext != ctx {
		t.Fatal("ordinary unprepared worker path changed", err)
	}
}

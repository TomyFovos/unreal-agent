package native

import (
	"bytes"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/mutation"
	"github.com/unreallabsai/unreal-agent/harness/operation"
)

func registeredPlanFault(t *testing.T, current operation.Operation, fault string) operation.Operation {
	t.Helper()
	if fault == "wrong-tool" {
		current.ToolName = "edit"
		return current
	}
	state, err := operation.DecodeRemoteJobState(current)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err = json.Unmarshal(state.Plan.Data, &fields); err != nil {
		t.Fatal(err)
	}
	switch fault {
	case "unknown":
		fields["grant"] = true
	case "version":
		fields["version"] = 0
	case "expected":
		delete(fields, "expected")
	case "weak-error":
		fields["content"] = 123
	default:
		t.Fatal("unknown synthetic fault")
	}
	state.Plan.Data, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	current.State, err = json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	return current
}

func TestNativeRegisteredPreparedPlanPreservesAsyncStrictFailures(t *testing.T) {
	for _, fault := range []string{"unknown", "wrong-tool", "version", "expected"} {
		t.Run(fault, func(t *testing.T) {
			h, ctx, root := recoveryBasisFixture(t)
			current := operation.WithPreparationRequired(basisWrite(t, "synthetic-registered-invalid", "must-not-exist"), true)
			current = registeredPlanFault(t, current, fault)
			before := current.State.Clone()
			if err := h.AddRemoteJobWithContext(ctx, current); err != nil {
				t.Fatal("strict-only failure became a synchronous Add failure", err)
			}
			var update operation.Operation
			select {
			case <-h.RemoteJobPreparations():
				t.Fatal("strict invalid plan requested owner ACK")
			case update = <-h.RemoteJobUpdates():
			case <-time.After(time.Second):
				t.Fatal("strict invalid plan did not fail asynchronously")
			}
			state, err := operation.DecodeRemoteJobState(update)
			var result Result
			if err != nil || json.Unmarshal(state.Handle, &result) != nil || update.Status != operation.StatusFailed || result.Mutation == nil || result.Mutation.Code != mutation.Denied || len(result.Mutation.Targets) != 1 || result.Mutation.Targets[0].Code != mutation.NotApplied {
				t.Fatal("strict refusal lost typed pre-effect evidence", err)
			}
			if !bytes.Equal(current.State, before) {
				t.Fatal("accepted state changed")
			}
			if _, err := os.Stat(filepath.Join(root, "must-not-exist")); !os.IsNotExist(err) {
				t.Fatal("invalid plan reached Mutation")
			}
		})
	}
}

func TestNativeRegisteredPreparedPlanKeepsSynchronousParseError(t *testing.T) {
	h, ctx, _ := recoveryBasisFixture(t)
	current := registeredPlanFault(t, operation.WithPreparationRequired(basisWrite(t, "synthetic-weak-error", "b"), true), "weak-error")
	state, err := operation.DecodeRemoteJobState(current)
	if err != nil {
		t.Fatal(err)
	}
	var request Request
	want := json.Unmarshal(state.Plan.Data, &request)
	got := h.AddRemoteJobWithContext(ctx, current)
	if want == nil || got == nil || reflect.TypeOf(got) != reflect.TypeOf(want) || got.Error() != want.Error() {
		t.Fatal("old weak parse error type/text/precedence changed", got, want)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	got = h.AddRemoteJobWithContext(ctx, current)
	if got == nil || got.Error() != want.Error() {
		t.Fatal("closed handler changed parse-error precedence", got)
	}
	unknown := registeredPlanFault(t, operation.WithPreparationRequired(basisWrite(t, "synthetic-closed", "b"), true), "unknown")
	if got = h.AddRemoteJobWithContext(ctx, unknown); got == nil || got.Error() != "handler is closed" {
		t.Fatal("strict-only failure changed closed-handler error", got)
	}
}

func TestNativeRegisteredPreparedPlanExecutesExactRequestAfterACK(t *testing.T) {
	h, ctx, root := recoveryBasisFixture(t)
	r := Request{Version: Version, Action: "write", Path: "b", Expected: &mutation.Revision{}, Content: strings.Repeat("<>&日本語\n", 4096)}
	spec, err := Spec(r)
	if err != nil {
		t.Fatal(err)
	}
	current := operation.WithPreparationRequired(operation.Operation{ID: "synthetic-registered-valid", ToolName: "write", Status: operation.StatusReady, Type: spec.Type, Version: spec.Version, State: spec.State, MaxOutputLength: spec.MaxOutputLength}, true)
	before := current.State.Clone()
	if err := h.AddRemoteJobWithContext(ctx, current); err != nil {
		t.Fatal(err)
	}
	var prepared operation.PreparationRequest
	select {
	case prepared = <-h.RemoteJobPreparations():
	case <-time.After(time.Second):
		t.Fatal("registered worker did not request ACK")
	}
	// Duplicate IDs retain the original active worker even when the second
	// proposal would fail strict preparation. This is a synthetic owner ACK;
	// existing Host tests separately establish durable disk acknowledgement.
	if err := h.AddRemoteJobWithContext(ctx, registeredPlanFault(t, current, "unknown")); err != nil {
		t.Fatal("duplicate Add changed", err)
	}
	if _, err := os.Stat(filepath.Join(root, r.Path)); !os.IsNotExist(err) {
		t.Fatal("mutation occurred before ACK")
	}
	select {
	case <-h.RemoteJobUpdates():
		t.Fatal("worker completed before ACK")
	case <-h.RemoteJobPreparations():
		t.Fatal("duplicate ID created another worker")
	default:
	}
	prepared.Result <- operation.PreparationResult{Operation: operation.WithExecutionBasis(current, prepared.Basis)}
	var update operation.Operation
	select {
	case update = <-h.RemoteJobUpdates():
	case <-time.After(time.Second):
		t.Fatal("worker did not finish after ACK")
	}
	state, err := operation.DecodeRemoteJobState(update)
	original, originalErr := operation.DecodeRemoteJobState(current)
	var result Result
	if err != nil || originalErr != nil || json.Unmarshal(state.Handle, &result) != nil || update.Status != operation.StatusCompleted || result.Mutation == nil || result.Mutation.Code != mutation.Applied || !bytes.Equal(state.Plan.Data, original.Plan.Data) || !bytes.Equal(current.State, before) {
		t.Fatal("prepared worker changed request or exact accepted plan", err, originalErr)
	}
	got, err := os.ReadFile(filepath.Join(root, r.Path))
	if err != nil || string(got) != r.Content || len(result.Mutation.Targets) != 1 || result.Mutation.Targets[0].Revision != mutation.RevisionOf(got) {
		t.Fatal("actual Mutation bytes/revision changed", err)
	}
}

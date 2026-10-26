package coordinator

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"reflect"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/mutation"
	files "github.com/unreallabsai/unreal-agent/harness/native"
	"github.com/unreallabsai/unreal-agent/harness/operation"
)

func recoveryReuseOperation(t *testing.T, action string) operation.Operation {
	t.Helper()
	request := files.Request{Version: files.Version, Action: action, Path: "synthetic-owner-target"}
	if action == "write" || action == "edit" {
		request.Expected = &mutation.Revision{}
		request.Content = "synthetic owner proposal"
		request.OldText, request.NewText = "old", "new"
	}
	spec, err := files.Spec(request)
	if err != nil {
		t.Fatal(err)
	}
	return operation.Operation{ID: "synthetic-owner-reuse", ToolName: action, Status: operation.StatusReady,
		Type: spec.Type, Version: spec.Version, State: spec.State, MaxOutputLength: spec.MaxOutputLength}
}

func recoveryReuseOwner(native bool) *coordinator {
	current := New(Dependencies{}).(*coordinator)
	if native {
		current.dependencies.LLM = conversationAdapter{}
	}
	return current
}

func TestRecoveryPreparationHeaderPreservesProviderAndToolScope(t *testing.T) {
	for _, tc := range []struct {
		name, action, tool string
		native, required   bool
	}{
		{"native-write", "write", "write", true, true},
		{"native-edit", "edit", "edit", true, true},
		{"native-unnamed", "write", "", true, true},
		{"native-read", "read", "read", true, false},
		{"other-provider", "write", "write", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := recoveryReuseOperation(t, tc.action)
			candidate.ToolName = tc.tool
			state := candidate.State.Clone()
			prepared, denial := recoveryReuseOwner(tc.native).prepareRecoveryOperation(t.Context(), candidate)
			if denial != nil || prepared.RequiresPreparation() != tc.required || !bytes.Equal(prepared.State, state) {
				t.Fatal("header changed provider/tool scope or accepted plan", denial)
			}
			if candidate.RequiresPreparation() {
				t.Fatal("header mutated the original canonical preparation flag")
			}
		})
	}
}

func TestRecoveryAlreadyRequiredPreparationRetainsDetachedMetadata(t *testing.T) {
	for _, malformed := range []bool{false, true} {
		t.Run(map[bool]string{false: "valid", true: "malformed-still-untrusted"}[malformed], func(t *testing.T) {
			candidate := recoveryReuseOperation(t, "write")
			if malformed {
				candidate.State = jsontext.Value(`{"Plan":`)
			}
			candidate.Execution = &operation.ExecutionMetadata{PreparationRequired: true,
				ExecutionBasis: jsontext.Value(`{"synthetic":"execution"}`), Dependencies: []operation.ID{"synthetic-parent"},
				RecoveryBasis: []operation.UncertainEffect{{ID: "synthetic-a", Basis: jsontext.Value(`{"synthetic":"recovery"}`)}}}
			candidate.Idempotence = &operation.IdempotencyMetadata{Idempotency: jsontext.Value(`{"synthetic":"idempotency"}`)}
			before := operation.CloneExecutionMetadata(candidate)
			prepared, denial := recoveryReuseOwner(true).prepareRecoveryOperation(t.Context(), candidate)
			if denial != nil || !operation.SameExecutionMetadata(prepared, before) || !bytes.Equal(prepared.State, before.State) {
				t.Fatal("already-required metadata or plan changed", denial)
			}
			if malformed {
				// A Host flag is never authority. Strict worker preparation rejects
				// this before it can use a Files service (the handler has none).
				if _, err := (&files.Handler{}).PrepareExecution(t.Context(), prepared); !errors.Is(err, operation.ErrIndependenceUnproven) {
					t.Fatal("already-required flag bypassed strict worker validation", err)
				}
			}
			prepared.Execution.PreparationRequired = false
			prepared.ExecutionBasis()[0] = '!'
			prepared.Execution.Dependencies[0] = "changed"
			prepared.Execution.RecoveryBasis[0].ID = "changed"
			prepared.Execution.RecoveryBasis[0].Basis[0] = '!'
			prepared.Idempotency()[0] = '!'
			if !operation.SameExecutionMetadata(candidate, before) {
				t.Fatal("returned metadata aliases the canonical input")
			}
		})
	}
}

type recoveryReusePlanner struct {
	*fakeOperationManager
	calls   int
	effects []operation.UncertainEffect
	err     error
}

func (p *recoveryReusePlanner) PlanRecovery(_ context.Context, candidate operation.Operation, effects []operation.UncertainEffect) (operation.Operation, error) {
	p.calls++
	p.effects = effects
	return operation.WithRecoveryBasis(candidate, effects), p.err
}

func TestRecoveryCandidateClassificationRetainsDependencyAndPlannerChecks(t *testing.T) {
	for _, tc := range []string{"finite-write", "observational-read", "missing-dependency", "planner-refusal"} {
		t.Run(tc, func(t *testing.T) {
			current := recoveryReuseOwner(true)
			state := current.conversationState()
			state.quarantined["synthetic-z"], state.quarantined["synthetic-a"] = true, true
			state.effects["synthetic-z"] = jsontext.Value(`{"effect":"z"}`)
			state.effects["synthetic-a"] = jsontext.Value(`{"effect":"a"}`)
			planner := &recoveryReusePlanner{fakeOperationManager: newFakeOperationManager()}
			current.dependencies.Operations = planner
			candidate := recoveryReuseOperation(t, "write")
			if tc == "observational-read" {
				candidate = recoveryReuseOperation(t, "read")
			}
			if tc == "missing-dependency" {
				candidate.Execution = &operation.ExecutionMetadata{Dependencies: []operation.ID{"missing"}}
			}
			if tc == "planner-refusal" {
				planner.err = operation.ErrIndependenceUnproven
			}
			before := candidate.State.Clone()
			prepared, denial := current.prepareRecoveryOperation(t.Context(), candidate)
			wantDenied := tc == "missing-dependency" || tc == "planner-refusal"
			if (denial != nil) != wantDenied || !bytes.Equal(candidate.State, before) || !bytes.Equal(prepared.State, before) {
				t.Fatal("candidate classification changed acceptance or immutable plan", denial)
			}
			wantCalls := 1
			if tc == "observational-read" || tc == "missing-dependency" {
				wantCalls = 0
			}
			if planner.calls != wantCalls || len(planner.adds) != 0 {
				t.Fatal("classification skipped a required check or dispatched an effect", planner.calls)
			}
			if wantCalls != 0 {
				want := []operation.UncertainEffect{{ID: "synthetic-a", Basis: state.effects["synthetic-a"]}, {ID: "synthetic-z", Basis: state.effects["synthetic-z"]}}
				if !reflect.DeepEqual(planner.effects, want) {
					t.Fatal("planner did not receive the sorted exact canonical effect bases")
				}
			}
			if !wantDenied && tc == "finite-write" {
				var raw map[string]jsontext.Value
				encoded, err := json.Marshal(prepared)
				if err != nil || json.Unmarshal(encoded, &raw) != nil || len(raw["RecoveryBasis"]) == 0 {
					t.Fatal("successful proposal lost flat canonical RecoveryBasis", err)
				}
			}
		})
	}
}

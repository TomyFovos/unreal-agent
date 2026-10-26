package coordinator

import (
	"bytes"
	"encoding/json/jsontext"
	"errors"
	"reflect"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/operation"
)

func preparedMetadataFixture() (operation.Operation, operation.Operation) {
	current := operation.Operation{ID: "synthetic-comparison", Execution: &operation.ExecutionMetadata{PreparationRequired: true}}
	basis := append([]byte("{\"synthetic\":\""), bytes.Repeat([]byte("x"), 600)...)
	basis = append(basis, []byte("\"}")...)
	return current, operation.WithExecutionBasis(current, jsontext.Value(basis))
}

func TestPreparedExecutionMetadataMatchesSetterOracle(t *testing.T) {
	current, prepared := preparedMetadataFixture()
	cases := []struct {
		name   string
		modify func(*operation.Operation, *operation.Operation)
	}{
		{"new_basis", func(_, _ *operation.Operation) {}},
		{"already_same_basis", func(c, p *operation.Operation) { *c = operation.WithExecutionBasis(*c, p.ExecutionBasis()) }},
		{"nil_execution", func(c, _ *operation.Operation) { c.Execution = nil }},
		{"empty_execution", func(c, p *operation.Operation) {
			c.Execution = &operation.ExecutionMetadata{}
			p.Execution = &operation.ExecutionMetadata{}
		}},
		{"both_nil", func(c, p *operation.Operation) { c.Execution = nil; p.Execution = nil }},
		{"nil_empty_basis", func(c, p *operation.Operation) {
			c.Execution.ExecutionBasis = nil
			p.Execution.ExecutionBasis = jsontext.Value{}
		}},
		{"preparation_changed", func(_, p *operation.Operation) { p.Execution.PreparationRequired = false }},
		{"dependencies_changed", func(_, p *operation.Operation) { p.Execution.Dependencies = []operation.ID{"other"} }},
		{"dependencies_same", func(c, p *operation.Operation) {
			c.Execution.Dependencies = []operation.ID{"a", "b"}
			p.Execution.Dependencies = []operation.ID{"a", "b"}
		}},
		{"dependencies_order", func(c, p *operation.Operation) {
			c.Execution.Dependencies = []operation.ID{"a", "b"}
			p.Execution.Dependencies = []operation.ID{"b", "a"}
		}},
		{"dependencies_nil_empty", func(_, p *operation.Operation) { p.Execution.Dependencies = []operation.ID{} }},
		{"recovery_same", func(c, p *operation.Operation) {
			c.Execution.RecoveryBasis = []operation.UncertainEffect{{ID: "prior", Basis: jsontext.Value("{\"n\":9007199254740993}")}}
			p.Execution.RecoveryBasis = operation.CloneExecutionMetadata(*c).RecoveryBasis()
		}},
		{"recovery_id_changed", func(c, p *operation.Operation) {
			c.Execution.RecoveryBasis = []operation.UncertainEffect{{ID: "prior"}}
			p.Execution.RecoveryBasis = []operation.UncertainEffect{{ID: "other"}}
		}},
		{"recovery_bytes_changed", func(c, p *operation.Operation) {
			c.Execution.RecoveryBasis = []operation.UncertainEffect{{ID: "prior", Basis: jsontext.Value("1.0")}}
			p.Execution.RecoveryBasis = []operation.UncertainEffect{{ID: "prior", Basis: jsontext.Value("1")}}
		}},
		{"recovery_nil_empty", func(_, p *operation.Operation) { p.Execution.RecoveryBasis = []operation.UncertainEffect{} }},
		{"idempotency_same", func(c, p *operation.Operation) {
			c.Idempotence = &operation.IdempotencyMetadata{Idempotency: jsontext.Value(" {\"x\":1} ")}
			p.Idempotence = &operation.IdempotencyMetadata{Idempotency: c.Idempotency().Clone()}
		}},
		{"idempotency_changed", func(_, p *operation.Operation) {
			p.Idempotence = &operation.IdempotencyMetadata{Idempotency: jsontext.Value("{\"x\":1}")}
		}},
		{"idempotency_nil_empty", func(_, p *operation.Operation) { p.Idempotence = &operation.IdempotencyMetadata{} }},
		// The existing setter/comparator use exact bytes; they are not JSON validators.
		{"nonminimal_basis", func(_, p *operation.Operation) {
			p.Execution.ExecutionBasis = jsontext.Value(" {\"s\":\"\\u0041\",\"n\":9007199254740993} ")
		}},
		{"duplicate_basis", func(_, p *operation.Operation) { p.Execution.ExecutionBasis = jsontext.Value("{\"x\":1,\"x\":2}") }},
		{"invalid_basis", func(_, p *operation.Operation) { p.Execution.ExecutionBasis = jsontext.Value("{unfinished") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, p := operation.CloneExecutionMetadata(current), operation.CloneExecutionMetadata(prepared)
			tc.modify(&c, &p)
			beforeC, beforeP := operation.CloneExecutionMetadata(c), operation.CloneExecutionMetadata(p)
			want := operation.SameExecutionMetadata(operation.WithExecutionBasis(c, p.ExecutionBasis()), p)
			if got := preparedExecutionMetadataMatches(c, p); got != want {
				t.Fatalf("setter comparison oracle differs: got %v want %v", got, want)
			}
			if !reflect.DeepEqual(c, beforeC) || !reflect.DeepEqual(p, beforeP) {
				t.Fatal("read-only comparison mutated its inputs")
			}
		})
	}
}

var preparedMetadataMatchSink bool

func TestPreparedExecutionMetadataComparisonDoesNotClone(t *testing.T) {
	current, prepared := preparedMetadataFixture()
	allocations := testing.AllocsPerRun(100, func() { preparedMetadataMatchSink = preparedExecutionMetadataMatches(current, prepared) })
	if !preparedMetadataMatchSink {
		t.Fatal("valid comparison rejected")
	}
	if allocations != 0 {
		t.Fatalf("comparison allocated %g objects; no owned value is published", allocations)
	}
}

func TestPreparedMetadataMismatchNeverApprovesOrSaves(t *testing.T) {
	cases := []struct {
		name   string
		modify func(*operation.Operation)
	}{
		{"tool", func(p *operation.Operation) { p.ToolName = "other" }},
		{"type", func(p *operation.Operation) { p.Type = "other" }},
		{"version", func(p *operation.Operation) { p.Version++ }},
		{"state", func(p *operation.Operation) { p.State = jsontext.Value("null") }},
		{"empty_basis", func(p *operation.Operation) { p.Execution.ExecutionBasis = nil }},
		{"required", func(p *operation.Operation) { p.Execution.PreparationRequired = false }},
		{"dependency", func(p *operation.Operation) { p.Execution.Dependencies = []operation.ID{"other"} }},
		{"recovery", func(p *operation.Operation) { p.Execution.RecoveryBasis = []operation.UncertainEffect{{ID: "other"}} }},
		{"idempotency", func(p *operation.Operation) {
			p.Idempotence = &operation.IdempotencyMetadata{Idempotency: jsontext.Value("1")}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, store, workspace := newBasisFaultFixture(t)
			manager := f.current.dependencies.Operations.(*basisFaultPreparer)
			statuses, err := f.current.handleModelResponse(manager.ctx, basisFaultResponse(f.current.state.currentTurnID))
			if err != nil || len(statuses) != 1 || len(statuses[0].Operations) != 1 {
				t.Fatal("canonical proposal unavailable", err)
			}
			accepted := operation.CloneExecutionMetadata(statuses[0].Operations[0])
			prepared, err := manager.handler.PrepareExecution(manager.ctx, accepted)
			if err != nil || len(prepared.ExecutionBasis()) == 0 {
				t.Fatal("actual native preparation missing", err)
			}
			before := len(store.savedOperations)
			tc.modify(&prepared)
			acks := 0
			var ackErr error
			err = f.current.commitOperationPreparation(manager.ctx, operation.PreparedOperation{Operation: prepared, Acknowledge: func(e error) { acks++; ackErr = e }})
			if !errors.Is(err, operation.ErrPreparationUncommitted) || acks != 1 || !errors.Is(ackErr, operation.ErrPreparationUncommitted) {
				t.Fatal("mismatch did not fail closed before ACK", err, acks, ackErr)
			}
			if len(store.savedOperations) != before || !reflect.DeepEqual(f.current.state.operations[accepted.ID], accepted) {
				t.Fatal("mismatch changed the canonical checkpoint")
			}
			basisFaultNoExecution(t, f, workspace)
		})
	}
}

func TestPreparedMetadataDifferentCommittedBasisCannotBeRebound(t *testing.T) {
	f, store, workspace := newBasisFaultFixture(t)
	manager := f.current.dependencies.Operations.(*basisFaultPreparer)
	statuses, err := f.current.handleModelResponse(manager.ctx, basisFaultResponse(f.current.state.currentTurnID))
	if err != nil || len(statuses) != 1 || len(statuses[0].Operations) != 1 {
		t.Fatal("canonical proposal unavailable", err)
	}
	accepted := statuses[0].Operations[0]
	prepared, err := manager.handler.PrepareExecution(manager.ctx, accepted)
	if err != nil || len(prepared.ExecutionBasis()) == 0 {
		t.Fatal("real native basis unavailable", err)
	}
	// The comparison substitutes the candidate basis. The separate existing-basis
	// guard must still prevent rebinding an already checkpointed operation.
	accepted = operation.WithExecutionBasis(accepted, jsontext.Value("{\"synthetic_other_basis\":true}"))
	f.current.state.operations[accepted.ID] = accepted
	if !preparedExecutionMetadataMatches(accepted, prepared) {
		t.Fatal("fixture does not reach the separate basis-conflict guard")
	}
	before := len(store.savedOperations)
	acks := 0
	var ackErr error
	err = f.current.commitOperationPreparation(manager.ctx, operation.PreparedOperation{Operation: prepared, Acknowledge: func(e error) { acks++; ackErr = e }})
	if !errors.Is(err, operation.ErrPreparationUncommitted) || acks != 1 || !errors.Is(ackErr, operation.ErrPreparationUncommitted) {
		t.Fatal("committed basis was rebound", err, acks, ackErr)
	}
	if len(store.savedOperations) != before || !reflect.DeepEqual(f.current.state.operations[accepted.ID], accepted) {
		t.Fatal("basis conflict changed canonical state")
	}
	basisFaultNoExecution(t, f, workspace)
}

func BenchmarkPreparedMetadataComparison(b *testing.B) {
	current, prepared := preparedMetadataFixture()
	b.Run("old_setter_oracle", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			preparedMetadataMatchSink = operation.SameExecutionMetadata(operation.WithExecutionBasis(current, prepared.ExecutionBasis()), prepared)
		}
	})
	b.Run("current_comparison", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			preparedMetadataMatchSink = preparedExecutionMetadataMatches(current, prepared)
		}
	})
}

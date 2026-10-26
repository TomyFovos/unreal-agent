package operation

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/permission"
)

type layoutBaselineFlatSpec struct {
	// Dependencies are Host-supplied execution prerequisites, never permission grants.
	Dependencies    []ID `json:",omitzero"`
	MaxOutputLength int  `json:",omitzero"`
	Type            Type
	Version         Version
	State           jsontext.Value `json:",omitzero"`
	Idempotency     jsontext.Value `json:",omitzero"`
}

type layoutBaselineFlatOperation struct {
	// ExecutionBasis binds the concrete executor footprint before canonical publication.
	// RecoveryBasis carries bounded uncertainty constraints; neither grants authority.
	ExecutionBasis jsontext.Value       `json:",omitzero"`
	RecoveryBasis  []UncertainEffect    `json:",omitzero"`
	Dependencies   []ID                 `json:",omitzero"`
	Permission     *permission.Decision `json:",omitzero"`
	// ToolName records origin for current-policy checks on resume; it is not a grant.
	ToolName        string            `json:",omitzero"`
	Denial          *permission.Error `json:",omitzero"`
	MaxOutputLength int               `json:",omitzero"`
	ID              ID
	Type            Type
	Version         Version
	Status          Status
	State           jsontext.Value `json:",omitzero"`
	Idempotency     jsontext.Value `json:",omitzero"`
}

func TestExecutionMetadataFlatCanonicalCompatibility(t *testing.T) {
	for _, name := range []string{"nil", "empty-container", "dependencies-only", "empty-slices", "idempotency-only", "full"} {
		t.Run(name, func(t *testing.T) {
			old := layoutBaselineFlatOperation{ID: "synthetic-layout", Type: TypeValue, Version: VersionValue, Status: StatusReady}
			current := Operation{ID: old.ID, Type: old.Type, Version: old.Version, Status: old.Status}
			switch name {
			case "empty-container":
				current.Execution = &ExecutionMetadata{}
				current.Idempotence = &IdempotencyMetadata{}
			case "dependencies-only":
				old.Dependencies = []ID{"prior"}
				current = WithDependencies(current, old.Dependencies)
			case "empty-slices":
				old.Dependencies = []ID{}
				old.RecoveryBasis = []UncertainEffect{}
				current = WithDependencies(current, old.Dependencies)
				current = WithRecoveryBasis(current, old.RecoveryBasis)
			case "idempotency-only":
				old.Idempotency = jsontext.Value(`{"key":"synthetic"}`)
				current = WithIdempotency(current, old.Idempotency)
			case "full":
				old.ExecutionBasis = jsontext.Value(`{"version":1}`)
				old.Dependencies = []ID{"prior"}
				old.RecoveryBasis = []UncertainEffect{{ID: "blocked", Basis: jsontext.Value(`{"version":1}`)}}
				current = WithExecutionBasis(current, old.ExecutionBasis)
				current = WithDependencies(current, old.Dependencies)
				current = WithRecoveryBasis(current, old.RecoveryBasis)
				old.Idempotency = jsontext.Value(`{"key":"synthetic"}`)
				current = WithIdempotency(current, old.Idempotency)
			}
			before, err := json.Marshal(old)
			if err != nil {
				t.Fatal(err)
			}
			after, err := json.Marshal(current)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("flat canonical bytes/order changed")
			}
			var restored Operation
			if err = json.Unmarshal(before, &restored, json.RejectUnknownMembers(true)); err != nil {
				t.Fatal("old flat record no longer decodes", err)
			}
			if !SameExecutionMetadata(current, restored) {
				t.Fatal("legacy flat record lost metadata")
			}
			if name == "nil" && restored.Execution != nil {
				t.Fatal("legacy no-metadata decode allocated metadata")
			}
			if old.Idempotency == nil && restored.Idempotence != nil {
				t.Fatal("legacy no-idempotency decode allocated metadata")
			}
			var legacy layoutBaselineFlatOperation
			if err = json.Unmarshal(after, &legacy, json.RejectUnknownMembers(true)); err != nil {
				t.Fatal("new record no longer decodes as baseline shape", err)
			}
		})
	}
}

func TestSpecPrerequisitesRemainFlatWithoutExecutorProof(t *testing.T) {
	for _, deps := range [][]ID{nil, {}, {"host-prior"}} {
		old := layoutBaselineFlatSpec{Dependencies: deps, Type: TypeValue, Version: VersionValue}
		current := WithPrerequisites(Spec{Type: old.Type, Version: old.Version}, deps)
		a, err := json.Marshal(old)
		if err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(current)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(a, b) {
			t.Fatal("Spec flat JSON changed")
		}
		var restored Spec
		if err = json.Unmarshal(a, &restored, json.RejectUnknownMembers(true)); err != nil {
			t.Fatal(err)
		}
		if deps == nil && restored.Prerequisites != nil {
			t.Fatal("ordinary legacy Spec gained allocation")
		}
	}
	for _, raw := range []string{`{"ExecutionBasis":{"version":1}}`, `{"RecoveryBasis":[]}`, `{"PreparationRequired":true}`} {
		var spec Spec
		if err := json.Unmarshal([]byte(raw), &spec, json.RejectUnknownMembers(true)); err == nil {
			t.Fatal("Spec accepted executor proof")
		}
	}
	spec := WithPrerequisites(Spec{}, []ID{"host-prior"})
	current := WithDependencies(Operation{}, spec.DependencyIDs())
	spec.DependencyIDs()[0] = "changed-spec"
	if current.DependencyIDs()[0] != "host-prior" || len(current.ExecutionBasis()) != 0 || len(current.RecoveryBasis()) != 0 {
		t.Fatal("Spec copied alias or invented executor proof")
	}
}

func TestExecutionMetadataNilAndCopyOnWrite(t *testing.T) {
	plain := Operation{}
	if CloneExecutionMetadata(plain).Execution != nil || plain.ExecutionBasis() != nil || plain.RecoveryBasis() != nil || plain.DependencyIDs() != nil {
		t.Fatal("nil fast path changed representation")
	}
	if !SameExecutionMetadata(plain, Operation{Execution: &ExecutionMetadata{}}) {
		t.Fatal("empty container changed prior semantic comparison")
	}
	if WithDependencies(plain, nil).Execution != nil || WithRecoveryBasis(plain, nil).Execution != nil || WithExecutionBasis(plain, nil).Execution != nil {
		t.Fatal("nil setter allocated container")
	}
	original := WithDependencies(Operation{}, []ID{"prior"})
	changed := WithExecutionBasis(original, jsontext.Value(`{"version":1}`))
	if changed.Execution == original.Execution {
		t.Fatal("setter retained mutable metadata container")
	}
	changed.DependencyIDs()[0] = "worker"
	if original.DependencyIDs()[0] != "prior" || len(original.ExecutionBasis()) != 0 {
		t.Fatal("copy-on-write modified canonical metadata")
	}
	worker := CloneExecutionMetadata(changed)
	worker.ExecutionBasis()[2] = 'X'
	if SameExecutionMetadata(changed, worker) || !bytes.Equal(changed.ExecutionBasis(), jsontext.Value(`{"version":1}`)) {
		t.Fatal("worker mutated comparison baseline")
	}
}

func TestExecutionMetadataBadFlatMembersAreRejected(t *testing.T) {
	for _, raw := range []string{`{"Dependencies":["a"],"Dependencies":["b"]}`, `{"ExecutionBasis":{},"ExecutionBasis":{}}`, `{"Dependencies":"scalar"}`, `{"RecoveryBasis":{}}`, `{"Execution":{"Dependencies":["a"]}}`, `{"Idempotency":{},"Idempotency":{}}`, `{"Idempotence":{"Idempotency":{}}}`, `{"PreparationRequired":"true"}`} {
		var value Operation
		if err := json.Unmarshal([]byte(raw), &value, json.RejectUnknownMembers(true)); err == nil {
			t.Fatal("invalid or nested metadata accepted")
		}
	}
	var value Operation
	if err := json.Unmarshal([]byte(`{"Dependencies":null}`), &value, json.RejectUnknownMembers(true)); err != nil {
		t.Fatal(err)
	}
	if !SameExecutionMetadata(value, Operation{}) {
		t.Fatal("null dependencies became a grant")
	}
}

package sessionstore

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

// This is the exact pre-optional ToolCallStatus JSON field arrangement.
type layoutBaselineFlatStatus struct {
	TurnID         session.TurnID
	CallID         string
	Status         tool.CallStatus
	Operations     []operation.Operation `json:",omitempty"`
	InheritedFacts []HistoricalOperation `json:",omitzero"`
}

func TestInheritedFactsFlatCanonicalCompatibility(t *testing.T) {
	for _, name := range []string{"absent", "empty-container", "empty-facts", "full"} {
		t.Run(name, func(t *testing.T) {
			old := layoutBaselineFlatStatus{TurnID: "synthetic-turn", CallID: "synthetic-call"}
			current := ToolCallStatus{TurnID: old.TurnID, CallID: old.CallID}
			switch name {
			case "empty-container":
				current.Inheritance = &InheritedOperationFacts{}
			case "empty-facts":
				old.InheritedFacts = []HistoricalOperation{}
				current = WithInheritedFacts(current, old.InheritedFacts)
			case "full":
				old.InheritedFacts = []HistoricalOperation{HistoricalOperation(operation.WithExecutionBasis(operation.Operation{ID: "synthetic-fact", Type: operation.TypeValue, Version: operation.VersionValue, Status: operation.StatusCompleted}, jsontext.Value(`{"version":1}`)))}
				current = WithInheritedFacts(current, old.InheritedFacts)
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
			var restored ToolCallStatus
			if err = json.Unmarshal(before, &restored, json.RejectUnknownMembers(true)); err != nil {
				t.Fatal("old flat status no longer decodes", err)
			}
			if len(restored.InheritedFacts()) != len(old.InheritedFacts) {
				t.Fatal("legacy inspection facts lost")
			}
			if name == "absent" && restored.Inheritance != nil {
				t.Fatal("ordinary status decode allocated inheritance")
			}
			var legacy layoutBaselineFlatStatus
			if err = json.Unmarshal(after, &legacy, json.RejectUnknownMembers(true)); err != nil {
				t.Fatal("new status no longer decodes as baseline shape", err)
			}
			if len(legacy.InheritedFacts) != len(old.InheritedFacts) {
				t.Fatal("backward inspection facts lost")
			}
		})
	}
}

func TestInheritedFactsNilAndDetachedForkProjection(t *testing.T) {
	plain := ToolCallStatus{}
	if plain.InheritedFacts() != nil || WithInheritedFacts(plain, nil).Inheritance != nil {
		t.Fatal("nil status gained allocation or facts")
	}
	original := operation.WithExecutionBasis(operation.Operation{ID: "synthetic-parent"}, jsontext.Value(`{"version":1}`))
	facts := []HistoricalOperation{HistoricalOperation(original)}
	child := WithInheritedFacts(plain, facts)
	child.InheritedFacts()[0].ID = "synthetic-child-copy"
	childOp := operation.Operation(child.InheritedFacts()[0])
	childOp.ExecutionBasis()[2] = 'X'
	if facts[0].ID != "synthetic-parent" || !bytes.Equal(original.ExecutionBasis(), jsontext.Value(`{"version":1}`)) {
		t.Fatal("fork projection mutated parent facts/metadata")
	}
	second := WithInheritedFacts(child, child.InheritedFacts())
	second.InheritedFacts()[0].ID = "another-copy"
	if child.InheritedFacts()[0].ID != "synthetic-child-copy" {
		t.Fatal("replacement reused inherited slice")
	}
}

func TestInheritedFactsInvalidNestedOrDuplicateMembers(t *testing.T) {
	for _, raw := range []string{`{"InheritedFacts":[],"InheritedFacts":[]}`, `{"InheritedFacts":"scalar"}`, `{"Inheritance":{"InheritedFacts":[]}}`} {
		var value ToolCallStatus
		if err := json.Unmarshal([]byte(raw), &value, json.RejectUnknownMembers(true)); err == nil {
			t.Fatal("invalid/nested inheritance accepted")
		}
	}
	var value ToolCallStatus
	if err := json.Unmarshal([]byte(`{"InheritedFacts":null}`), &value, json.RejectUnknownMembers(true)); err != nil || len(value.InheritedFacts()) != 0 {
		t.Fatal("legacy null facts changed semantics")
	}
}

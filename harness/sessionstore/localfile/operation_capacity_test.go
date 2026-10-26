package localfile

import (
	"bytes"
	"encoding/json/jsontext"
	"fmt"
	"reflect"
	"testing"
	"unsafe"

	"github.com/unreallabsai/unreal-agent/harness/operation"
)

func TestSessionHeadOperationCapacityGrowth(t *testing.T) {
	input := make([]operation.Operation, 10000)
	for i := range input {
		input[i] = operation.WithPreparationRequired(operation.WithIdempotency(operation.WithExecutionBasis(operation.Operation{ID: operation.ID(fmt.Sprint(i)), State: jsontext.Value(`{"state":true}`)}, jsontext.Value(`{"basis":true}`)), jsontext.Value(`{"key":true}`)), true)
	}
	preserved := append([]operation.Operation(nil), input...)
	var head sessionHead
	var reference []operation.Operation
	capacities := []int{}
	maxCombined := 0
	for i, value := range input {
		old := head.Operations
		var oldValues []operation.Operation
		if len(old) == cap(old) {
			oldValues = append([]operation.Operation(nil), old...)
		}
		head.appendOperations(value)
		reference = append(reference, value)
		if len(head.Operations) != i+1 || !reflect.DeepEqual(head.Operations[i], reference[i]) {
			t.Fatalf("value/order mismatch at %d", i)
		}
		if oldValues != nil && !reflect.DeepEqual(old, oldValues) {
			t.Fatalf("prior backing values changed at %d", i)
		}
		if cap(head.Operations) != cap(old) {
			capacities = append(capacities, cap(head.Operations))
			combined := cap(old) + cap(head.Operations)
			if combined > maxCombined {
				maxCombined = combined
			}
		}
		if cap(head.Operations) > 1024 && cap(head.Operations) > (3*len(head.Operations)+1)/2 {
			t.Fatalf("capacity bound exceeded at %d", i)
		}
		if !operation.SameExecutionMetadata(head.Operations[i], value) || !bytes.Equal(head.Operations[i].State, value.State) {
			t.Fatal("metadata changed")
		}
	}
	if !reflect.DeepEqual(head.Operations, reference) {
		t.Fatal("final order/values changed")
	}
	if !reflect.DeepEqual(input, preserved) {
		t.Fatal("input changed")
	}
	expected := []int{1, 2, 4, 8, 16, 32, 64, 128, 256, 512, 1024, 1536, 2304, 3456, 5184, 7776, 11664}
	if !reflect.DeepEqual(capacities, expected) {
		t.Fatalf("capacities %v", capacities)
	}
	// The helper copies Operation values, preserving the existing shallow pointer
	// contract. It introduces neither deep cloning nor aliasing into input slices.
	if head.Operations[0].Execution != input[0].Execution || head.Operations[0].Idempotence != input[0].Idempotence {
		t.Fatal("metadata pointer contract changed")
	}
	head.Operations[0].ID = "changed"
	if input[0].ID != preserved[0].ID {
		t.Fatal("input slice backing aliased")
	}
	t.Logf("Operation bytes=%d capacity sequence=%v maxCombinedBackingElements=%d maxCombinedBackingBytes=%d", unsafe.Sizeof(operation.Operation{}), capacities, maxCombined, uintptr(maxCombined)*unsafe.Sizeof(operation.Operation{}))
}

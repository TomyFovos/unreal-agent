package operation

import (
	"encoding/json/jsontext"
	"testing"
)

func TestIdempotencyNilAndCopyOnWrite(t *testing.T) {
	plain := Operation{}
	if plain.Idempotency() != nil || WithIdempotency(plain, nil).Idempotence != nil {
		t.Fatal("ordinary operation acquired optional metadata")
	}
	if !SameExecutionMetadata(plain, Operation{Execution: &ExecutionMetadata{}, Idempotence: &IdempotencyMetadata{}}) {
		t.Fatal("empty metadata changed the operation")
	}
	key := jsontext.Value(`{"key":"original"}`)
	original := WithIdempotency(WithDependencies(plain, []ID{"prior"}), key)
	key[2] = 'X'
	if string(original.Idempotency()) != `{"key":"original"}` {
		t.Fatal("setter retained input idempotency bytes")
	}
	prepared := CloneExecutionMetadata(original)
	if prepared.Execution == original.Execution || prepared.Idempotence == original.Idempotence {
		t.Fatal("preparation setter retained mutable containers")
	}
	prepared.Idempotency()[2] = 'Y'
	prepared.DependencyIDs()[0] = "worker"
	if string(original.Idempotency()) != `{"key":"original"}` || original.DependencyIDs()[0] != "prior" {
		t.Fatal("preparation setter changed canonical metadata")
	}
	rekeyed := WithIdempotency(original, jsontext.Value(`{"key":"other"}`))
	rekeyed.DependencyIDs()[0] = "other"
	if original.DependencyIDs()[0] != "prior" || string(original.Idempotency()) != `{"key":"original"}` {
		t.Fatal("idempotency setter retained canonical aliases")
	}

}

package operation

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"
)

func TestIdempotencyAndPreparationNilAndCopyOnWrite(t *testing.T) {
	plain := Operation{}
	if plain.Idempotency() != nil || plain.RequiresPreparation() || WithIdempotency(plain, nil).Idempotence != nil || WithPreparationRequired(plain, false).Execution != nil {
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
	prepared := WithPreparationRequired(original, true)
	if original.RequiresPreparation() || prepared.Execution == original.Execution || prepared.Idempotence == original.Idempotence {
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
	cleared := WithPreparationRequired(prepared, false)
	if !prepared.RequiresPreparation() || cleared.RequiresPreparation() || SameExecutionMetadata(prepared, cleared) {
		t.Fatal("preparation removal was not detached and observable")
	}
}

func TestPreparationRequiredHasFlatOptionalWireField(t *testing.T) {
	original := WithIdempotency(Operation{ID: "synthetic", Type: TypeValue, Version: VersionValue, Status: StatusReady}, jsontext.Value(`{"key":"original"}`))
	before, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	prepared := WithPreparationRequired(original, true)
	raw, err := json.Marshal(prepared)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(raw, []byte(`{"PreparationRequired":true,`)) || !bytes.HasSuffix(raw, []byte(`,"Idempotency":{"key":"original"}}`)) {
		t.Fatal("optional fields lost flat canonical names/order", string(raw))
	}
	var restored Operation
	if err = json.Unmarshal(raw, &restored, json.RejectUnknownMembers(true)); err != nil || !restored.RequiresPreparation() || !SameExecutionMetadata(prepared, restored) {
		t.Fatal("preparation constraint did not round-trip", err)
	}
	after, err := json.Marshal(WithPreparationRequired(prepared, false))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("false preparation field changed prior canonical bytes", err)
	}
}

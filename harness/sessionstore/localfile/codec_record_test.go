package localfile

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

// legacyRecordBytes is the frozen 2c55 encoder, kept as a compatibility oracle.
func legacyRecordBytes(kind recordType, value any) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(logRecord{Type: kind, Data: data})
	if err != nil {
		return nil, err
	}
	if bytes.IndexByte(encoded, '\n') >= 0 {
		return nil, fmt.Errorf("encoded record contains a newline")
	}
	return append(encoded, '\n'), nil
}

func assertRecordCompatibility(t *testing.T, kind recordType, value any) []byte {
	t.Helper()
	want, wantErr := legacyRecordBytes(kind, value)
	got, err := encodeRecord(kind, value)
	if (err == nil) != (wantErr == nil) {
		t.Fatalf("error=%v; legacy=%v", err, wantErr)
	}
	if err != nil {
		if fmt.Sprintf("%T:%v", err, err) != fmt.Sprintf("%T:%v", wantErr, wantErr) {
			t.Fatalf("error semantics differ: %v; legacy %v", err, wantErr)
		}
		return nil
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("record bytes differ (got%d want%d)", len(got), len(want))
	}
	if len(got) == 0 || got[len(got)-1] != '\n' || bytes.Count(got, []byte{'\n'}) != 1 {
		t.Fatal("record is not exactly one JSONL line")
	}
	return got
}

func TestEncodeRecordCanonicalCompatibility(t *testing.T) {
	plain := validOperation("operation-1", operation.StatusReady)
	metadata := operation.WithIdempotency(operation.WithDependencies(plain, []operation.ID{"prior"}), jsontext.Value(`{"nonce":"<>&\u2028\u2029"}`))
	tests := []struct {
		name  string
		kind  recordType
		value any
	}{
		{"session", recordSession, sessionRecord{Version: formatVersion, Session: session.Session{ID: "session-1", CreatedAt: stateCreatedAt}}},
		{"operation_nil_metadata", recordOperation, operationRecord{Operation: plain}},
		{"operation_optional_metadata", recordOperation, operationRecord{Operation: metadata}},
		{"item_turn", recordItem, itemRecord{Item: sessionstore.Item{Sequence: 1, RecordedAt: stateUpdatedAt, Kind: sessionstore.ItemTurn, Data: session.Turn{ID: "turn-1", Type: session.TurnRegular}}}},
		{"item_model_response", recordItem, itemRecord{Item: sessionstore.Item{Sequence: 2, RecordedAt: stateUpdatedAt, Kind: sessionstore.ItemModelResponse, Data: sessionstore.ModelResponse{TurnID: "turn-1", Response: llm.Response{Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: strings.Repeat("<>&日本語\n", 8192)}}}}}}}},
		{"item_tool_status", recordItem, itemRecord{Item: sessionstore.Item{Sequence: 3, RecordedAt: stateUpdatedAt, Kind: sessionstore.ItemToolCallStatus, Data: sessionstore.ToolCallStatus{TurnID: "turn-1", CallID: "call-1", Status: tool.CallStatus{WaitingFor: []operation.ID{metadata.ID}}}}, Operations: []operation.Operation{metadata}}},

		{"opaque_object_order", recordOperation, jsontext.Value(`{ "z":-0, "a":1.2300e+007, "s":"\u0061\/\b\f\n\r\t<>&\u2028\u2029日本語" }`)},
		{"null", recordItem, nil},
		{"nil_raw", recordItem, jsontext.Value(nil)},
		{"array", recordOperation, jsontext.Value(`[null,true,false,-0,9007199254740993,1e-200]`)},
		{"float_negative_zero", recordOperation, math.Copysign(0, -1)},
		{"nonfinite", recordOperation, math.Inf(1)},
		{"unknown_kind", recordType("unsupported"), operationRecord{Operation: plain}},
		{"kind_escapes", recordType("quote\"slash\\newline\n日本語<>&\u2028"), operationRecord{Operation: plain}},
		{"kind_invalid_utf8", recordType(string([]byte{0xff})), operationRecord{Operation: plain}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) { assertRecordCompatibility(t, test.kind, test.value) })
	}
	for _, raw := range []string{`{"x":1,"x":2}`, `{"x":1,"\u0078":2}`, `{`, `[0,]`, `01`, `true false`, "\"\xff\""} {
		t.Run(fmt.Sprintf("invalid_%x", []byte(raw)), func(t *testing.T) {
			op := plain
			op.State = jsontext.Value(raw)
			if _, err := legacyRecordBytes(recordOperation, operationRecord{Operation: op}); err == nil {
				t.Fatal("legacy unexpectedly accepted malformed state")
			}
			assertRecordCompatibility(t, recordOperation, operationRecord{Operation: op})
		})
	}
}

func TestEncodeRecordCombinedDepthLimit(t *testing.T) {
	for _, depth := range []int{9996, 9997, 9998, 10000} {
		t.Run(fmt.Sprint(depth), func(t *testing.T) {
			op := validOperation("deep", operation.StatusReady)
			op.State = jsontext.Value(strings.Repeat("[", depth) + "0" + strings.Repeat("]", depth))
			assertRecordCompatibility(t, recordOperation, operationRecord{Operation: op})
		})
	}
	// A standalone raw value at depth10000 is valid alone but invalid inside the
	// canonical envelope. Optimizing by blindly splicing it must not bypass this.
	raw := jsontext.Value(strings.Repeat("[", 10000) + "0" + strings.Repeat("]", 10000))
	if _, err := json.Marshal(raw); err != nil {
		t.Fatal("standalone boundary fixture invalid", err)
	}
	if _, err := legacyRecordBytes(recordOperation, raw); err == nil {
		t.Fatal("legacy envelope unexpectedly allows over-depth JSON")
	}
	assertRecordCompatibility(t, recordOperation, raw)
}

type recordDepthAwareValue struct{}

func (recordDepthAwareValue) MarshalJSONTo(enc *jsontext.Encoder) error {
	return enc.WriteToken(jsontext.Int(int64(enc.StackDepth())))
}

type recordCountingValue struct{ calls *int }

func (v recordCountingValue) MarshalJSONTo(enc *jsontext.Encoder) error {
	*v.calls++
	return enc.WriteToken(jsontext.Int(int64(enc.StackDepth())))
}

func TestEncodeRecordRejectsNestedCustomDataBeforeMarshal(t *testing.T) {
	for _, nestedModelData := range []bool{false, true} {
		t.Run(fmt.Sprint(nestedModelData), func(t *testing.T) {
			calls := 0
			custom := recordCountingValue{calls: &calls}
			item := sessionstore.Item{Sequence: 1, RecordedAt: stateUpdatedAt, Kind: sessionstore.ItemTurn, Data: custom}
			if nestedModelData {
				item.Kind = sessionstore.ItemModelResponse
				item.Data = sessionstore.ModelResponse{TurnID: "turn-1", Response: llm.Response{Output: []llm.Item{{Type: llm.ItemMessage, Data: custom}}}}
			}
			value := itemRecord{Item: item}
			if _, err := legacyRecordBytes(recordItem, value); err == nil {
				t.Fatal("legacy unexpectedly accepted nested custom data")
			}
			assertRecordCompatibility(t, recordItem, value)
			if calls != 0 {
				t.Fatalf("nested custom marshaler invoked %d times before typed-data rejection", calls)
			}
		})
	}
}

func TestEncodeRecordCustomValueKeepsStandaloneMarshalContext(t *testing.T) {
	// This extension point is deliberately outside the canonical DTO fast path.
	// Moving all arbitrary values under a new envelope would change this value.
	encoded := assertRecordCompatibility(t, recordItem, recordDepthAwareValue{})
	if !bytes.Contains(encoded, []byte(`"data":0`)) {
		t.Fatalf("custom marshaler context changed:%s", encoded)
	}
	assertRecordCompatibility(t, recordItem, &recordDepthAwareValue{})
}

func TestEncodeRecordPointerValuesKeepCompatibility(t *testing.T) {
	header := sessionRecord{Version: formatVersion, Session: session.Session{ID: "session-1", CreatedAt: stateCreatedAt}}
	item := itemRecord{Item: sessionstore.Item{Sequence: 1, RecordedAt: stateUpdatedAt, Kind: sessionstore.ItemTurn, Data: session.Turn{ID: "turn-1", Type: session.TurnRegular}}}
	update := operationRecord{Operation: validOperation("pointer", operation.StatusReady)}
	// The fast path accepts only the three exact value types. Pointers and
	// arbitrary/custom values keep the established two-stage marshal path.
	assertRecordCompatibility(t, recordSession, &header)
	assertRecordCompatibility(t, recordItem, &item)
	assertRecordCompatibility(t, recordOperation, &update)
	assertRecordCompatibility(t, recordOperation, (*operationRecord)(nil))
}

func TestEncodeRecordOwnsOutputAndPreservesInput(t *testing.T) {
	raw := jsontext.Value(`{"body":"日本語<>&\n","n":1.2300e+07}`)
	op := operation.WithIdempotency(validOperation("owned", operation.StatusReady), raw)
	op.State = raw
	snapshot := operation.CloneExecutionMetadata(op)
	snapshot.State = raw.Clone()
	encoded := assertRecordCompatibility(t, recordOperation, operationRecord{Operation: op})
	expected := append([]byte(nil), encoded...)
	if !reflect.DeepEqual(op, snapshot) {
		t.Fatal("encoder mutated input operation")
	}
	for i := range raw {
		raw[i] = ' '
	}
	if !bytes.Equal(encoded, expected) {
		t.Fatal("encoded record aliases input raw JSON")
	}
	encoded[0] = '!'
	if bytes.Contains(raw, []byte{'!'}) {
		t.Fatal("input aliases output storage")
	}
}

func TestEncodeRecordGoldenAndTornReaderCompatibility(t *testing.T) {
	raw := emptyLog(t)
	state, size, err := decodeLog(raw)
	if err != nil || size != int64(len(raw)) || state.Snapshot.Session.ID != "session-1" {
		t.Fatal("valid initial record rejected", err)
	}
	header := sessionRecord{Version: formatVersion, Session: state.Snapshot.Session}
	encoded := assertRecordCompatibility(t, recordSession, header)
	legacy, err := legacyRecordBytes(recordSession, header)
	if err != nil || !bytes.Equal(encoded, legacy) {
		t.Fatal("header mismatch", err)
	}
	for _, readerInput := range [][]byte{encoded, append(append([]byte(nil), encoded...), []byte(`{"type":"operation","data":`)...)} {
		decoded, committed, err := decodeLog(readerInput)
		if err != nil || committed != int64(len(encoded)) || decoded.Snapshot.Session.ID != "session-1" {
			t.Fatal("committed/torn boundary changed", err, committed)
		}
	}
	for _, bad := range []string{`{"type":"session","type":"session","data":{}}`, `{"type":"unknown","data":{}}`, `{"type":"operation","data":{"Operation":{"ID":"x","ID":"x"}}}`} {
		if _, _, err := decodeLog(append(append([]byte(nil), encoded...), append([]byte(bad), '\n')...)); err == nil {
			t.Fatal("duplicate or unknown completed record accepted")
		}
	}
}

func FuzzEncodeRecordCanonicalFrame(f *testing.F) {
	for _, raw := range []string{`null`, `{}`, `[]`, `{"s":"<>&\u2028\u2029日本語"}`, `{"z":-0,"a":1.2300e+007}`, `{"x":1,"\u0078":2}`, `{`} {
		f.Add(raw)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		if len(raw) > 64<<10 {
			t.Skip("bounded compatibility corpus")
		}
		op := validOperation("fuzz", operation.StatusReady)
		op.State = jsontext.Value(raw)
		assertRecordCompatibility(t, recordOperation, operationRecord{Operation: op})
	})
}

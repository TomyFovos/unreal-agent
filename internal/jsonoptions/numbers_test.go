package jsonoptions_test

import (
	jsonv1 "encoding/json"
	"encoding/json/v2"
	"testing"

	"github.com/unreallabsai/unreal-agent/internal/jsonoptions"
)

func TestPreserveNumbers(t *testing.T) {
	const input = `{"empty":{},"integer":9007199254740993,"nested":[null,true,"text",{"decimal":0.1234567890123456789,"large":1e1000}]}`
	var value map[string]any
	if err := json.Unmarshal([]byte(input), &value, jsonoptions.PreserveNumbers()); err != nil {
		t.Fatal(err)
	}
	if number, ok := value["integer"].(jsonv1.Number); !ok || number.String() != "9007199254740993" {
		t.Fatalf("integer = %#v", value["integer"])
	}
	nested, ok := value["nested"].([]any)
	if !ok || len(nested) != 4 || nested[0] != nil || nested[1] != true || nested[2] != "text" {
		t.Fatalf("nested values changed: %#v", value["nested"])
	}
	object, ok := nested[3].(map[string]any)
	if !ok {
		t.Fatalf("nested object type = %T", nested[3])
	}
	for key, want := range map[string]jsonv1.Number{"decimal": "0.1234567890123456789", "large": "1e1000"} {
		if number, ok := object[key].(jsonv1.Number); !ok || number != want {
			t.Fatalf("%s = %#v", key, object[key])
		}
	}
	encoded, err := json.Marshal(value, json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != input {
		t.Fatalf("round trip changed JSON:\n got %s\nwant %s", encoded, input)
	}
}

func TestPreserveNumbersRejectsInvalidJSON(t *testing.T) {
	for _, input := range []string{`{"number":01}`, `{"number":1e}`, `{"number":NaN}`, `{"number":1,"number":2}`} {
		var value map[string]any
		if err := json.Unmarshal([]byte(input), &value, jsonoptions.PreserveNumbers()); err == nil {
			t.Fatalf("accepted invalid JSON: %s", input)
		}
	}
}

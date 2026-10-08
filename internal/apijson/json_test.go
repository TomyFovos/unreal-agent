package apijson_test

import (
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/internal/apijson"
)

func TestDecoderPreservesNumbersAndJSONKinds(t *testing.T) {
	const input = `{"array":[null,true,"日本語🌙",9007199254740993],"decimal":0.1234567890123456789,"object":{},"large":1e1000}`
	var value map[string]any
	if err := apijson.NewDecoder(strings.NewReader(input)).Decode(&value); err != nil {
		t.Fatal(err)
	}
	array, ok := value["array"].([]any)
	if !ok || len(array) != 4 || array[0] != nil || array[1] != true || array[2] != "日本語🌙" {
		t.Fatal("JSON kinds changed")
	}
	if n, ok := array[3].(apijson.Number); !ok || n.String() != "9007199254740993" {
		t.Fatal("decoder rounded the integer")
	}
	for key, want := range map[string]string{"decimal": "0.1234567890123456789", "large": "1e1000"} {
		if n, ok := value[key].(apijson.Number); !ok || n.String() != want {
			t.Fatalf("decoder changed %s", key)
		}
	}
	if _, ok := value["object"].(map[string]any); !ok {
		t.Fatal("object became an array or scalar")
	}
}

func TestJSONEscapingAndInvalidInputs(t *testing.T) {
	value := map[string]any{"text": "日本語👩‍💻 </script> \" \\ \n\t\x1b\u2028", "n": apijson.Number("9007199254740993")}
	body, err := apijson.Marshal(value)
	if err != nil || !apijson.Valid(body) {
		t.Fatalf("escaped JSON invalid: %v", err)
	}
	var restored map[string]any
	if err := apijson.Unmarshal(body, &restored); err != nil || restored["text"] != value["text"] || restored["n"] != value["n"] {
		t.Fatal("escaped content or number did not round trip")
	}
	if _, err := apijson.Marshal(map[string]any{"text": "bad\xff"}); err == nil {
		t.Fatal("invalid UTF-8 was accepted")
	}
	for _, body := range []string{`{"n":1,"n":2}`, `{"n":1e}`, `{} true`, `{"n":NaN}`} {
		var restored any
		if err := apijson.Unmarshal([]byte(body), &restored); err == nil {
			t.Fatal("malformed JSON was accepted")
		}
	}
}

package tool

import (
	"strings"
	"testing"
)

func TestConciseDescriptionsRetainToolContracts(t *testing.T) {
	wants := map[string][]string{
		ReadName:  {"whole-file SHA-256 revision", "range reads", "Binary files"},
		WriteName: {"shared mutation lock", "Full Access", "Absolute paths", "parent directory must exist", "expected:{exists:false}"},
		EditName:  {"Full Access", "expected whole-file revision", "replace_all:true", "stale edits"},
	}
	for _, d := range nativeDefinitions() {
		for _, want := range wants[d.Tool.Name] {
			if !strings.Contains(d.Tool.Description, want) {
				t.Fatalf("%s lacks %s", d.Tool.Name, want)
			}
		}
	}
	description := maxOutputLengthSchema()["description"].(string)
	for _, want := range []string{"per text field", "head/tail", "omitted count", "full-stream path", "40000"} {
		if !strings.Contains(description, want) {
			t.Fatal(description)
		}
	}
}

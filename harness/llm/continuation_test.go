package llm

import (
	"errors"
	"strings"
	"testing"
)

func TestContinuationValidation(t *testing.T) {
	call := Item{Type: ItemToolCall, Data: ToolCall{CallID: "synthetic", Name: "read", Arguments: `{"path":"fixture"}`}}
	result := Item{Type: ItemToolResult, Data: ToolResult{CallID: "synthetic"}}
	conflict := call
	conflict.Data = ToolCall{CallID: "synthetic", Name: "write", Arguments: "PRIVATE_ARGUMENT"}
	for _, tc := range []struct {
		name  string
		input []Item
		code  string
	}{
		{"plain", nil, ""}, {"paired", []Item{call, result}, ""}, {"running-final", []Item{call, result, result}, ""},
		{"output-first", []Item{result, call}, "native_output_without_call"}, {"missing", []Item{call}, "native_call_without_output"},
		{"conflict", []Item{call, result, conflict, result}, "conflicting_native_call"}, {"bad-call", []Item{{Type: ItemToolCall}}, "invalid_native_call"},
		{"bad-result", []Item{{Type: ItemToolResult}}, "invalid_native_output"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateContinuation(tc.input)
			if tc.code == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var typed *ContinuationError
			if !errors.As(err, &typed) || typed.Code != tc.code || strings.Contains(err.Error(), "PRIVATE_ARGUMENT") || strings.Contains(err.Error(), "synthetic") {
				t.Fatal("unsafe or incorrect continuation diagnostic")
			}
		})
	}
}

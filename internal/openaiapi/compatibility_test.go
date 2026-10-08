package openaiapi_test

import (
	"encoding/json/jsontext"
	"reflect"
	"testing"

	"github.com/unreallabsai/unreal-agent/internal/apijson"
	"github.com/unreallabsai/unreal-agent/internal/openaiapi"
)

type wireUnion interface {
	MarshalJSON() ([]byte, error)
	UnmarshalJSON([]byte) error
	Discriminator() (string, error)
	ValueByDiscriminator() (any, error)
}

// The variants are the pinned specification's wire values, including aliases.
// These are codec tests, not registrations or authority to execute these tools.
func TestEveryToolDiscriminatorRoundTrips(t *testing.T) {
	checkUnionVariants(t, func() wireUnion { return &openaiapi.Tool{} }, map[string]string{
		"function": "FunctionTool", "file_search": "FileSearchTool",
		"computer": "ComputerTool", "computer_use_preview": "ComputerUsePreviewTool",
		"web_search": "WebSearchTool", "web_search_2025_08_26": "WebSearchTool",
		"mcp": "MCPTool", "code_interpreter": "CodeInterpreterTool",
		"programmatic_tool_calling": "ProgrammaticToolCallingParam", "image_generation": "ImageGenTool",
		"local_shell": "LocalShellToolParam", "shell": "FunctionShellToolParam",
		"custom": "CustomToolParam", "namespace": "NamespaceToolParam",
		"tool_search": "ToolSearchToolParam", "apply_patch": "ApplyPatchToolParam",
		"web_search_preview": "WebSearchPreviewTool", "web_search_preview_2025_03_11": "WebSearchPreviewTool",
	})
}

func TestEveryOutputDiscriminatorRoundTrips(t *testing.T) {
	checkUnionVariants(t, func() wireUnion { return &openaiapi.OutputItem{} }, map[string]string{
		"message": "OutputMessage", "file_search_call": "FileSearchToolCall",
		"function_call": "FunctionToolCall", "function_call_output": "FunctionToolCallOutputResource",
		"web_search_call": "WebSearchToolCall", "computer_call": "ComputerToolCall",
		"computer_call_output": "ComputerToolCallOutputResource", "reasoning": "ReasoningItem",
		"program": "Program", "program_output": "ProgramOutput",
		"tool_search_call": "ToolSearchCall", "tool_search_output": "ToolSearchOutput",
		"additional_tools": "AdditionalTools", "compaction": "CompactionBody",
		"image_generation_call": "ImageGenToolCall", "code_interpreter_call": "CodeInterpreterToolCall",
		"local_shell_call": "LocalShellToolCall", "local_shell_call_output": "LocalShellToolCallOutput",
		"shell_call": "FunctionShellCall", "shell_call_output": "FunctionShellCallOutput",
		"apply_patch_call": "ApplyPatchToolCall", "apply_patch_call_output": "ApplyPatchToolCallOutput",
		"mcp_call": "MCPToolCall", "mcp_list_tools": "MCPListTools",
		"mcp_approval_request": "MCPApprovalRequest", "mcp_approval_response": "MCPApprovalResponseResource",
		"custom_tool_call": "CustomToolCall", "custom_tool_call_output": "CustomToolCallOutputResource",
	})
}

func checkUnionVariants(t *testing.T, fresh func() wireUnion, variants map[string]string) {
	t.Helper()
	for wire, name := range variants {
		t.Run(wire, func(t *testing.T) {
			union := fresh()
			if err := apijson.Unmarshal([]byte(`{"type":"`+wire+`"}`), union); err != nil {
				t.Fatal(err)
			}
			selected, err := union.ValueByDiscriminator()
			if err != nil || reflect.TypeOf(selected).Name() != name {
				t.Fatalf("selected %T, error=%v; want %s", selected, err, name)
			}
			for _, method := range []string{"From", "Merge"} {
				result := reflect.ValueOf(union).MethodByName(method + name).Call([]reflect.Value{reflect.ValueOf(selected)})
				if !result[0].IsNil() {
					t.Fatal(result[0].Interface())
				}
				got, err := union.Discriminator()
				if err != nil || got != wire {
					t.Fatalf("%s changed wire discriminator to %q: %v", method, got, err)
				}
			}
		})
	}
}

func TestGeneratedUnionShapesAndInvalidDiscriminators(t *testing.T) {
	for name, input := range map[string]string{
		"missing": `{}`, "null": `null`, "array": `[]`, "scalar": `42`,
		"wrong discriminator type": `{"type":42}`, "unknown": `{"type":"not_a_tool"}`,
	} {
		t.Run(name, func(t *testing.T) {
			var tool openaiapi.Tool
			if err := apijson.Unmarshal([]byte(input), &tool); err == nil {
				if _, err := tool.ValueByDiscriminator(); err == nil {
					t.Fatal("invalid discriminator selected a variant")
				}
			}
		})
	}
	for _, input := range []string{
		`{"type":"function","type":"mcp"}`, `{"type":"function",`, `{} []`,
		`{"type":"function","parameters":{"n":01}}`,
	} {
		var tool openaiapi.Tool
		if err := apijson.Unmarshal([]byte(input), &tool); err == nil {
			t.Fatalf("accepted malformed union: %s", input)
		}
	}
	var tool openaiapi.Tool
	if err := apijson.Unmarshal([]byte(`{"type":"function","name":7}`), &tool); err != nil {
		t.Fatal(err)
	}
	if _, err := tool.ValueByDiscriminator(); err == nil {
		t.Fatal("wrong concrete field type was accepted")
	}
}

func TestGeneratedScalarAndArrayVariants(t *testing.T) {
	for name, input := range map[string]string{
		"string": `"text"`, "number": `0.25`, "boolean": `true`, "array": `["text",0.25]`,
	} {
		t.Run(name, func(t *testing.T) {
			var value openaiapi.ComparisonFilter_Value
			if err := apijson.Unmarshal([]byte(input), &value); err != nil {
				t.Fatal(err)
			}
			assertWireJSON(t, value, input)
			var err error
			switch name {
			case "string":
				_, err = value.AsComparisonFilterValue0()
			case "number":
				_, err = value.AsComparisonFilterValue1()
			case "boolean":
				_, err = value.AsComparisonFilterValue2()
			case "array":
				_, err = value.AsComparisonFilterValue3()
			}
			if err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, input := range []string{`"hello"`, `[{"role":"user","content":"hello"}]`} {
		assertRoundTrip[openaiapi.InputParam](t, input)
	}
}

func TestGeneratedUnknownFieldPolicy(t *testing.T) {
	const input = `{"type":"function","name":"lookup","strict":false,"parameters":{"extra":{"n":9007199254740993}},"future_field":{"n":9007199254740993}}`
	var raw openaiapi.Tool
	if err := apijson.Unmarshal([]byte(input), &raw); err != nil {
		t.Fatal(err)
	}
	// Raw unions retain provider extensions; concrete views discard unknowns.
	assertWireJSON(t, raw, input)
	function, err := raw.AsFunctionTool()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := apijson.Marshal(function)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]jsontext.Value
	if err := apijson.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	if _, exists := fields["future_field"]; exists {
		t.Fatal("ordinary generated structs started retaining unknown fields")
	}
	if string(fields["parameters"]) != `{"extra":{"n":9007199254740993}}` {
		t.Fatal("arbitrary schema map lost unknown nested data or number precision")
	}
}

func TestGeneratedNullAndAbsentBoundaries(t *testing.T) {
	for _, input := range []string{`{}`, `{"instructions":null}`} {
		var response openaiapi.Response
		if err := apijson.Unmarshal([]byte(input), &response); err != nil {
			t.Fatal(err)
		}
		if response.Instructions != nil {
			t.Fatal("nullable pointer representation changed")
		}
		encoded, err := apijson.Marshal(response)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]jsontext.Value
		if err := apijson.Unmarshal(encoded, &fields); err != nil {
			t.Fatal(err)
		}
		if string(fields["instructions"]) != "null" {
			t.Fatal("required nullable field was omitted")
		}
	}
	for _, input := range []string{`{"model":"gpt-test"}`, `{"model":"gpt-test","instructions":null}`} {
		var request openaiapi.CreateResponse
		if err := apijson.Unmarshal([]byte(input), &request); err != nil {
			t.Fatal(err)
		}
		assertWireJSON(t, request, `{"model":"gpt-test"}`)
	}
}

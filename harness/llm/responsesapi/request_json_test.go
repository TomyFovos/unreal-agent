package responsesapi

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/internal/openaiapi"
)

func TestRequestBodyOmitsEmptyInput(t *testing.T) {
	body, err := requestBody(llm.Request{Model: llm.Model{ID: "gpt-test"}}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	if _, exists := fields["input"]; exists {
		t.Fatalf("empty input was included: %s", body)
	}
	if string(fields["store"]) != "false" {
		t.Fatalf("store = %s, want false", fields["store"])
	}
	if _, exists := fields["tools"]; exists {
		t.Fatalf("absent tools were included: %s", body)
	}
}

func TestSetUnionOmitsEmptyValues(t *testing.T) {
	var tool openaiapi.Tool
	if err := setUnion(&tool, openaiapi.FunctionTool{
		Type: "function", Name: "empty", Description: new(""),
		AllowedCallers: new([]openaiapi.CallableToolAllowedCaller{}),
		OutputSchema:   new(map[string]any{}),
		Parameters:     new(map[string]any{}), Strict: new(false),
	}); err != nil {
		t.Fatal(err)
	}
	encoded, err := tool.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"name":"empty","parameters":{},"strict":false,"type":"function"}`
	if string(encoded) != want {
		t.Fatalf("optional empty values changed:\n got %s\nwant %s", encoded, want)
	}
}

func TestToolResultOmitsEmptyCallID(t *testing.T) {
	item, err := requestInputItem(llm.Item{
		Type: llm.ItemToolResult,
		Data: llm.ToolResult{
			Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "done"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := item.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"output":[{"text":"done","type":"input_text"}],"type":"function_call_output"}`
	if string(encoded) != want {
		t.Fatalf("tool result = %s, want %s", encoded, want)
	}
}

func TestRequestBodyPreservesToolParameterNumbers(t *testing.T) {
	parameters := map[string]any{
		"const": int64(9007199254740993),
		"properties": map[string]any{
			"ratio": map[string]any{"minimum": jsontext.Value(`0.1234567890123456789`)},
			"large": map[string]any{"maximum": jsontext.Value(`1e1000`)},
		},
	}
	body, err := requestBody(llm.Request{
		Tools: []llm.Tool{{Type: llm.ToolFunction, Name: "precise", Parameters: parameters}},
	}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		Tools []struct {
			Parameters jsontext.Value `json:"parameters"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatal(err)
	}
	const want = `{"const":9007199254740993,"properties":{"large":{"maximum":1e1000},"ratio":{"minimum":0.1234567890123456789}}}`
	if len(request.Tools) != 1 || string(request.Tools[0].Parameters) != want {
		t.Fatalf("parameters lost numeric precision: %s", body)
	}
}

func TestToolParametersSurviveUnionDecodingAndRequestEncoding(t *testing.T) {
	const parameters = `{"properties":{"id":{"const":9007199254740993},"ratio":{"minimum":0.1234567890123456789},"size":{"maximum":1e1000}},"type":"object"}`
	var wireTool openaiapi.Tool
	if err := json.Unmarshal([]byte(`{"type":"function","name":"precise","strict":false,"parameters":`+parameters+`}`), &wireTool); err != nil {
		t.Fatal(err)
	}
	function, err := wireTool.AsFunctionTool()
	if err != nil {
		t.Fatal(err)
	}
	if function.Parameters == nil {
		t.Fatal("parameters are missing")
	}
	request, err := requestTool(llm.Tool{
		Type: llm.ToolFunction, Name: function.Name, Parameters: *function.Parameters,
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := request.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Parameters jsontext.Value `json:"parameters"`
	}
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if string(got.Parameters) != parameters {
		t.Fatalf("parameters changed:\n got %s\nwant %s", got.Parameters, parameters)
	}
}

func TestRequestBodyReportsToolParameterEncodingError(t *testing.T) {
	_, err := requestBody(llm.Request{
		Tools: []llm.Tool{{Type: llm.ToolFunction, Name: "invalid", Parameters: map[string]any{
			"properties": jsontext.Value(`{"broken":`),
		}}},
	}, "", nil)
	if err == nil || !strings.HasPrefix(err.Error(), `tool 0:`) {
		t.Fatalf("error = %v, want a tool encoding error", err)
	}
}

package openaiapi_test

import (
	"bytes"
	jsonv1 "encoding/json"
	"encoding/json/v2"
	"fmt"
	"reflect"
	"testing"

	"github.com/unreallabsai/unreal-agent/internal/apijson"
	"github.com/unreallabsai/unreal-agent/internal/openaiapi"
)

func TestResponseInstructionsAndRequiredNulls(t *testing.T) {
	for _, instructions := range []string{`"You are helpful."`, `[]`, `null`} {
		t.Run(instructions, func(t *testing.T) {
			assertRoundTrip[openaiapi.Response](t, fmt.Sprintf(`{
				"id":"resp_1","object":"response","created_at":0,
				"error":null,"incomplete_details":null,"instructions":%s,"metadata":null,
				"model":"gpt-4o","output":[],"parallel_tool_calls":true,
				"tool_choice":"auto","tools":[],"temperature":null,"top_p":null
			}`, instructions))
		})
	}
}

func TestNullableToolUnions(t *testing.T) {
	for _, input := range []string{
		`{"type":"mcp","server_label":"test","allowed_tools":["lookup"],"require_approval":"never"}`,
		`{"type":"mcp","server_label":"test","require_approval":{"always":{"tool_names":["lookup"]}}}`,
		`{"type":"mcp","server_label":"test","allowed_tools":{"read_only":true}}`,
	} {
		assertRoundTrip[openaiapi.MCPTool](t, input)
	}
	assertRoundTrip[openaiapi.FunctionShellToolParam](t, `{"type":"shell","environment":{"type":"local"}}`)
	assertRoundTrip[openaiapi.FunctionShellCallItemParam](t, `{
		"type":"shell_call","call_id":"c","environment":{"type":"local"},
		"action":{"commands":["pwd"]}
	}`)
	assertRoundTrip[openaiapi.FunctionShellCall](t, `{
		"type":"shell_call","id":"s","call_id":"c","status":"completed","environment":{"type":"local"},
		"action":{"commands":["pwd"],"max_output_length":null,"timeout_ms":null}
	}`)
}

func TestOptionalEmptyValuesAreOmitted(t *testing.T) {
	var request openaiapi.CreateResponse
	if err := apijson.Unmarshal([]byte(`{
		"model":"gpt-4o","input":"","instructions":"","tools":[],"metadata":{},
		"stream":false,"include":[],"max_output_tokens":0
	}`), &request); err != nil {
		t.Fatal(err)
	}
	assertWireJSON(t, request, `{"model":"gpt-4o","stream":false,"max_output_tokens":0}`)
}

func TestUnsetOptionalNullableFieldsAreOmitted(t *testing.T) {
	for _, input := range []string{`{}`, `{"instructions":null,"stream":null,"include":null}`} {
		var request openaiapi.CreateResponse
		if err := apijson.Unmarshal([]byte(input), &request); err != nil {
			t.Fatal(err)
		}
		assertWireJSON(t, request, `{}`)
	}
}

func TestArbitraryJSONPreservesNumbers(t *testing.T) {
	const schema = `{"properties":{"id":{"const":9007199254740993},"ratio":{"minimum":0.1234567890123456789}},"additionalProperties":false}`
	assertRoundTrip[openaiapi.ResponseFormatJsonSchemaSchema](t, schema)
	assertRoundTrip[openaiapi.EmptyModelParam](t, `{"id":9007199254740993}`)
	assertRoundTrip[openaiapi.FunctionTool](t,
		`{"type":"function","name":"lookup","strict":false,"parameters":`+schema+`,"output_schema":`+schema+`}`)
	assertRoundTrip[openaiapi.MCPListToolsTool](t,
		`{"name":"lookup","annotations":{"id":9007199254740993},"input_schema":`+schema+`}`)
	assertRoundTrip[openaiapi.ToolSearchCall](t, `{
		"type":"tool_search_call","id":"s","call_id":null,"execution":"server","status":"completed",
		"arguments":{"id":9007199254740993,"ratio":0.1234567890123456789}
	}`)
}

func TestToolUnionPreservesNumbers(t *testing.T) {
	const parameters = `{"integer":9007199254740993,"decimal":0.1234567890123456789,"large":1e1000}`
	const input = `{"type":"function","name":"lookup","strict":false,"description":"","allowed_callers":[],"parameters":` + parameters + `}`
	const want = `{"type":"function","name":"lookup","strict":false,"parameters":` + parameters + `}`
	var tool openaiapi.Tool
	if err := json.Unmarshal([]byte(input), &tool); err != nil {
		t.Fatal(err)
	}
	function, err := tool.AsFunctionTool()
	if err != nil {
		t.Fatal(err)
	}
	if function.Parameters == nil {
		t.Fatal("parameters are missing")
	}
	for key, want := range map[string]jsonv1.Number{
		"integer": "9007199254740993", "decimal": "0.1234567890123456789", "large": "1e1000",
	} {
		if got := (*function.Parameters)[key]; got != want {
			t.Fatalf("%s = %#v (%T), want json.Number(%q)", key, got, got, want)
		}
	}
	selected, err := tool.ValueByDiscriminator()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(selected, function) {
		t.Fatalf("discriminator selected %#v, want %#v", selected, function)
	}
	if err := tool.FromFunctionTool(function); err != nil {
		t.Fatal(err)
	}
	assertWireJSON(t, tool, want)
	if err := tool.MergeFunctionTool(function); err != nil {
		t.Fatal(err)
	}
	assertWireJSON(t, tool, want)
}

func TestToolDiscriminatorUsesWireValue(t *testing.T) {
	var tool openaiapi.Tool
	if err := tool.FromFunctionTool(openaiapi.FunctionTool{
		Type: "function", Name: "lookup",
		Strict: new(false), Parameters: new(map[string]any{}),
	}); err != nil {
		t.Fatal(err)
	}
	assertWireJSON(t, tool, `{"type":"function","name":"lookup","strict":false,"parameters":{}}`)
	value, err := tool.ValueByDiscriminator()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := value.(openaiapi.FunctionTool); !ok {
		t.Fatalf("unexpected tool type %T", value)
	}
	if err := tool.MergeFunctionTool(openaiapi.FunctionTool{
		Type: "function", Name: "updated",
		Strict: new(false), Parameters: new(map[string]any{}),
	}); err != nil {
		t.Fatal(err)
	}
	assertWireJSON(t, tool, `{"type":"function","name":"updated","strict":false,"parameters":{}}`)
}

func TestOutputAndStreamDiscriminatorsUseWireValues(t *testing.T) {
	var output openaiapi.OutputItem
	if err := output.FromOutputMessage(openaiapi.OutputMessage{
		Type: "message", Id: "msg_1", Role: "assistant", Status: "completed",
		Content: []openaiapi.OutputMessageContent{},
	}); err != nil {
		t.Fatal(err)
	}
	assertWireJSON(t, output, `{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[]}`)
	value, err := output.ValueByDiscriminator()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := value.(openaiapi.OutputMessage); !ok {
		t.Fatalf("unexpected output type %T", value)
	}
	var event openaiapi.ResponseStreamEvent
	if err := event.FromResponseTextDeltaEvent(openaiapi.ResponseTextDeltaEvent{
		Type: "response.output_text.delta", ItemId: "msg_1", SequenceNumber: 1,
		Delta: "hi", Logprobs: []openaiapi.ResponseLogProb{},
	}); err != nil {
		t.Fatal(err)
	}
	assertWireJSON(t, event, `{
		"type":"response.output_text.delta","item_id":"msg_1","sequence_number":1,
		"content_index":0,"output_index":0,"delta":"hi","logprobs":[]
	}`)
	value, err = event.ValueByDiscriminator()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := value.(openaiapi.ResponseTextDeltaEvent); !ok {
		t.Fatalf("unexpected event type %T", value)
	}
}

func TestOverlappingInputVariantsPreserveTheirType(t *testing.T) {
	var item openaiapi.Item
	if err := item.FromOutputMessage(openaiapi.OutputMessage{
		Type: "message", Id: "msg_1", Role: "assistant", Status: "completed",
		Content: []openaiapi.OutputMessageContent{},
	}); err != nil {
		t.Fatal(err)
	}
	var input openaiapi.InputItem
	if err := input.FromItem(item); err != nil {
		t.Fatal(err)
	}
	assertWireJSON(t, input, `{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[]}`)
}

func TestRecursiveFiltersPreserveTheirOperators(t *testing.T) {
	var value openaiapi.ComparisonFilter_Value
	if err := value.FromComparisonFilterValue0("document"); err != nil {
		t.Fatal(err)
	}
	var comparison openaiapi.CompoundFilter_Filters_Item
	if err := comparison.FromComparisonFilter(openaiapi.ComparisonFilter{Type: "ne", Key: "kind", Value: value}); err != nil {
		t.Fatal(err)
	}
	var nested openaiapi.CompoundFilter_Filters_Item
	if err := nested.FromCompoundFilter(openaiapi.CompoundFilter{
		Type: "or", Filters: []openaiapi.CompoundFilter_Filters_Item{comparison},
	}); err != nil {
		t.Fatal(err)
	}
	assertWireJSON(t, nested, `{"type":"or","filters":[{"type":"ne","key":"kind","value":"document"}]}`)
	for _, filter := range []openaiapi.CompoundFilter_Filters_Item{comparison, nested} {
		if _, err := filter.ValueByDiscriminator(); err != nil {
			t.Fatal(err)
		}
	}
}

func assertRoundTrip[T any](t *testing.T, input string) {
	t.Helper()
	for _, unmarshal := range []func([]byte, any) error{
		func(data []byte, value any) error {
			decoder := jsonv1.NewDecoder(bytes.NewReader(data))
			decoder.UseNumber()
			return decoder.Decode(value)
		},
		apijson.Unmarshal,
	} {
		var value T
		if err := unmarshal([]byte(input), &value); err != nil {
			t.Fatal(err)
		}
		assertWireJSON(t, value, input)
	}
}

func assertWireJSON(t *testing.T, value any, want string) {
	t.Helper()
	got, err := apijson.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decodeJSON(t, got), decodeJSON(t, []byte(want))) {
		t.Errorf("got %s\nwant %s", got, want)
	}
}

func decodeJSON(t *testing.T, data []byte) any {
	t.Helper()
	decoder := jsonv1.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

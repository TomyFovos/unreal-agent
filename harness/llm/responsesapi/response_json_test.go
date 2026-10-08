package responsesapi

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/tool/native"
)

func TestResponseAcceptsAllInstructionWireVariantsWithoutPublicReplay(t *testing.T) {
	for _, instructions := range []string{`"private instructions"`, `[]`, `[{"role":"developer","content":"private instructions"}]`, `null`} {
		response, err := decodeResponse([]byte(`{"id":"response-1","status":"completed","output":[],"instructions":` + instructions + `}`))
		if err != nil {
			t.Fatal(err)
		}
		if len(response.Output) != 0 || response.ID != "response-1" || response.Stop != llm.StopComplete {
			t.Fatal("instructions were projected as public output")
		}
	}
}

func TestResponsePreservesExactNestedArgumentsAndLargeIntegerUsage(t *testing.T) {
	arguments := ` {"path":42,"nested":[null,true,"日本語👩‍💻",9007199254740993,{"decimal":0.1234567890123456789}]} `
	encoded, err := json.Marshal(arguments)
	if err != nil {
		t.Fatal(err)
	}
	response, err := decodeResponse([]byte(`{
		"id":"response-1","status":"completed","future_field":{"n":9007199254740993},
		"output":[{"type":"function_call","call_id":"call-1","name":"read","arguments":` + string(encoded) + `}],
		"usage":{"input_tokens":9007199254740993,"input_tokens_details":{"cached_tokens":0},"output_tokens":0,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":9007199254740993}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if response.Usage.InputTokens != 9007199254740993 || len(response.Output) != 1 {
		t.Fatal("integer usage or response shape changed")
	}
	call := response.Output[0].Data.(llm.ToolCall)
	if call.Arguments != arguments {
		t.Fatal("provider argument string was reinterpreted or rounded")
	}
	ctx := &replayToolContext{}
	status := native.New("read").Translate(ctx, call)
	if status.Error == "" || ctx.submissions != 0 || len(status.WaitingFor) != 0 {
		t.Fatal("invalid arguments gained execution authority")
	}
	item, err := requestInputItem(response.Output[0])
	if err != nil {
		t.Fatal(err)
	}
	wire, err := item.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var replay struct {
		Arguments string `json:"arguments"`
	}
	if err := json.Unmarshal(wire, &replay); err != nil || replay.Arguments != arguments {
		t.Fatal("canonical replay changed syntactically valid arguments")
	}
}

func TestResponseMalformedAndUnsupportedUnionsRemainRejected(t *testing.T) {
	for name, body := range map[string]string{
		"array output item":       `{"status":"completed","output":[[]]}`,
		"scalar output item":      `{"status":"completed","output":[1]}`,
		"null output item":        `{"status":"completed","output":[null]}`,
		"unknown tool kind":       `{"status":"completed","output":[{"type":"unknown_tool"}]}`,
		"foreign MCP kind":        `{"status":"completed","output":[{"type":"mcp_call"}]}`,
		"wrong argument type":     `{"status":"completed","output":[{"type":"function_call","arguments":{}}]}`,
		"duplicate discriminator": `{"status":"completed","output":[{"type":"function_call","type":"message"}]}`,
		"unknown status":          `{"status":"unexpected","output":[]}`,
		"trailing JSON":           `{"status":"completed","output":[]} true`,
	} {
		t.Run(name, func(t *testing.T) {
			response, err := decodeResponse([]byte(body))
			if err == nil || len(response.Output) != 0 {
				t.Fatal("malformed/unsupported wire data produced a public response")
			}
		})
	}
}

func TestRequestWireEscapingAndRawExtensionPrecision(t *testing.T) {
	text := "CJK日本語 👩‍💻 </script> \" \\ \n\t\x1b"
	request := validRequest()
	request.Input[0].Data = llm.Message{Role: llm.RoleUser, Text: text}
	body, err := requestBody(request, "fixture-cache", map[string]jsontext.Value{"fixture_extension": jsontext.Value(`{"n":9007199254740993,"values":[null,true,{},[]]}`)})
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Input []struct {
			Content string `json:"content"`
		} `json:"input"`
		PromptCacheKey string         `json:"prompt_cache_key"`
		Extension      jsontext.Value `json:"fixture_extension"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Input) != 1 || wire.Input[0].Content != text || wire.PromptCacheKey != "fixture-cache" || string(wire.Extension) != `{"n":9007199254740993,"values":[null,true,{},[]]}` {
		t.Fatal("wire escaping, cache affinity or extension values changed")
	}
}

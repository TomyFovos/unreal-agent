//go:build linux || darwin

package claudecode

import (
	"bufio"
	"context"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

func TestStructuredReportedTokenStreamAndAPIDurationWithoutPrivateContent(t *testing.T) {
	stream := diagnosticInit +
		`{"type":"stream_event","event":{"type":"message_start","message":{"id":"private-helper-id","model":"synthetic-model","content":[],"usage":{"input_tokens":10,"cache_read_input_tokens":2,"cache_creation_input_tokens":0,"output_tokens":0}}}}` + "\n" +
		`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"serializer","name":"StructuredOutput","input":{}}}}` + "\n" +
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"private\":\"sensitive-argument\"}"}}}` + "\n" +
		`{"type":"stream_event","event":{"type":"content_block_stop","index":0}}` + "\n" +
		`{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":20}}}` + "\n" +
		`{"type":"stream_event","event":{"type":"message_stop"}}` + "\n" +
		`{"type":"assistant","message":` + sdkSerializerMessage + `}` + "\n" +
		strings.Replace(diagnosticFinal, `"is_error":false`, `"is_error":false,"duration_api_ms":2000,"usage":{"input_tokens":10,"cache_read_input_tokens":2,"cache_creation_input_tokens":0,"output_tokens":20}`, 1)
	var progress []llm.Progress
	proposal, response, err := readStructuredStream(bufio.NewScanner(strings.NewReader(stream)), diagnosticSchema(t), nil, nil, func(p llm.Progress) { progress = append(progress, p) })
	if err != nil || proposal.Final == nil || len(response.Output) != 0 || len(progress) != 2 {
		t.Fatal("structured stream statistics", err, progress)
	}
	if *progress[0].Tokens != (llm.TokenCounts{Input: 12, Output: 0, InputKnown: true, OutputKnown: true}) || *progress[1].Tokens != (llm.TokenCounts{Output: 20, OutputKnown: true}) {
		t.Fatal(progress)
	}
	if response.Generation == nil || response.Generation.Source != "provider_api" || response.Generation.DurationMicros != 2_000_000 {
		t.Fatal(response.Generation)
	}
	if value, known := response.Generation.OutputTPS(); !known || value != 10 {
		t.Fatal(value, known)
	}
	encoded, _ := json.Marshal(progress)
	for _, bad := range []string{"sensitive-argument", "private-helper-id", "StructuredOutput", "partial_json"} {
		if strings.Contains(string(encoded), bad) {
			t.Fatal("private stream body leaked through counters")
		}
	}
	badStream := strings.Replace(stream, `"name":"StructuredOutput"`, `"name":"Bash"`, 1)
	if _, _, err := readStructuredStream(bufio.NewScanner(strings.NewReader(badStream)), diagnosticSchema(t), nil, nil, func(llm.Progress) {}); err == nil {
		t.Fatal("statistics enabled side-effecting tool")
	}
}

func TestTextOnlyStatisticsDurationAbsentInvalidAndTokensUnknown(t *testing.T) {
	for _, duration := range []string{"", `,"duration_api_ms":0`, `,"duration_api_ms":-1`, `,"duration_api_ms":86400001`, `,"duration_api_ms":250`} {
		stream := strings.Replace(diagnosticInit, `["StructuredOutput"]`, `[]`, 1) + `{"type":"result","subtype":"success","result":"public","usage":{"input_tokens":3,"output_tokens":5}` + duration + "}\n"
		r, err := parseStream(strings.NewReader(stream), llm.RequestOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if (r.Generation != nil) != (duration == `,"duration_api_ms":250`) {
			t.Fatal("invalid duration invented a rate", duration, r.Generation)
		}
	}
	for _, wire := range []*wireUsage{nil, {Output: new(int64(4))}, {Input: new(int64(-1))}} {
		var got []llm.Progress
		emitTokenProgress(wireEvent{Type: "message_delta", Usage: wire}, func(p llm.Progress) { got = append(got, p) })
		if wire == nil || wire.Input != nil && *wire.Input < 0 {
			if len(got) != 0 {
				t.Fatal("invalid or absent usage emitted", got)
			}
			continue
		}
		if len(got) != 1 || got[0].Tokens.InputKnown || !got[0].Tokens.OutputKnown || got[0].Tokens.Output != 4 {
			t.Fatal("partial counters invented input", got)
		}
	}
}

func TestStructuredReplayUsageDoesNotInflateGenerationRate(t *testing.T) {
	r := newLayerRounds(t, layerRead, layerRead, layerFinal)
	i := 0
	response, err := runStructuredRounds(t.Context(), r.request, r.options, r.schema, r.limits,
		func(_ context.Context, _ llm.Model, schema *actionSchema, _, _ string) (actionProposal, llm.Response, error) {
			p, err := schema.parse([]byte(r.frames[i]))
			i++
			return p, llm.Response{Usage: llm.Usage{InputTokens: 12, OutputTokens: 20}, Generation: &llm.GenerationStatistics{Source: "provider_api", DurationMicros: 2_000_000, Tokens: llm.TokenCounts{Input: 12, Output: 20, InputKnown: true, OutputKnown: true}}}, err
		})
	if err != nil || len(r.calls) != 1 || response.Usage.OutputTokens != 40 || response.Generation.Tokens.Output != 20 {
		t.Fatal("duplicate inflated rate or executed twice", response, err)
	}
	if value, ok := response.Generation.OutputTPS(); !ok || value != 10 {
		t.Fatal(value, ok)
	}
}

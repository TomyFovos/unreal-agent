package responsesapi

import (
	"context"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

func TestReportedTokenProgressPartialUnknownAndLargePrecision(t *testing.T) {
	for _, tc := range []struct {
		usage string
		want  llm.TokenCounts
		emit  bool
	}{
		{`{"input_tokens":9007199254740993,"output_tokens":8}`, llm.TokenCounts{Input: 9007199254740993, Output: 8, InputKnown: true, OutputKnown: true}, true},
		{`{"output_tokens":0}`, llm.TokenCounts{OutputKnown: true}, true},
		{`null`, llm.TokenCounts{}, false},
		{`{"input_tokens":-2,"output_tokens":8}`, llm.TokenCounts{Output: 8, OutputKnown: true}, true},
	} {
		var got []llm.Progress
		ctx := context.WithValue(t.Context(), progressKey{}, func(p llm.Progress) { got = append(got, p) })
		ctx = context.WithValue(ctx, attemptKey{}, uint64(3))
		emitTextProgress(ctx, []byte(`{"type":"response.in_progress","response":{"id":"private","usage":`+tc.usage+`}}`))
		if !tc.emit {
			if len(got) != 0 {
				t.Fatal(got)
			}
			continue
		}
		if len(got) != 1 || got[0].Tokens == nil || *got[0].Tokens != tc.want || got[0].Attempt != 3 || got[0].Delta != "" {
			t.Fatal(got)
		}
		encoded, _ := json.Marshal(got)
		if strings.Contains(string(encoded), "private") {
			t.Fatal("provider identifier leaked")
		}
	}
	for _, usage := range []string{"", `,"usage":null`, `,"usage":{"input_tokens":9}`} {
		r, err := decodeResponse([]byte(`{"id":"r","status":"completed","output":[]` + usage + `}`))
		if err != nil {
			t.Fatal(err)
		}
		known := llm.ReportedTokens(r.Usage)
		if known.OutputKnown || known.InputKnown != (usage == `,"usage":{"input_tokens":9}`) {
			t.Fatal("missing counter interpreted as zero", r.Usage, known)
		}
	}
}

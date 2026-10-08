package claudecode

import (
	"encoding/json/v2"
	"errors"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"math"
	"slices"
	"strings"
	"testing"
)

const initRecord = textOnlyInit
const goodResult = `{"type":"result","subtype":"success","is_error":false,"result":"hello","usage":{"input_tokens":10,"cache_read_input_tokens":20,"cache_creation_input_tokens":30,"output_tokens":40},"total_cost_usd":123,"account_id":"account-secret"}` + "\n"

func requireCode(t *testing.T, err error, code string) {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("error=%v; want code=%s", err, code)
	}
}

func TestStreamTextUsageAndPrivateReasoning(t *testing.T) {
	s := initRecord + `{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"private-thinking-secret"}}}` + "\n" + `{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}}` + "\n" + `{"type":"assistant","message":{"id":"msg_1","model":"claude-configured-a","content":[{"type":"thinking","thinking":"private-thinking-secret","signature":"signature-secret"},{"type":"text","text":"hello"}],"stop_reason":"end_turn"}}` + "\n" + goodResult
	var draft string
	r, err := parseStream(strings.NewReader(s), llm.RequestOptions{Progress: func(p llm.Progress) { draft += p.Delta }})
	if err != nil {
		t.Fatal(err)
	}
	if draft != "hello" || len(r.Output) != 1 || r.Output[0].Data.(llm.Message).Text != "hello" || r.Model != "claude-configured-a" || r.ID != "msg_1" {
		t.Fatal(r, draft)
	}
	u := r.Usage
	if u.InputTokens != 60 || u.CachedInputTokens != 20 || u.CacheWriteInputTokens != 30 || u.OutputTokens != 40 || !slices.Equal(u.Unknown, []llm.UsageField{llm.UsageReasoning}) {
		t.Fatal(u)
	}
	b, _ := json.Marshal(r)
	for _, secret := range []string{"private-thinking-secret", "signature-secret", "account-secret", "total_cost_usd"} {
		if strings.Contains(string(b), secret) || strings.Contains(draft, secret) {
			t.Fatal("private stream field leaked")
		}
	}
}

func TestStreamRejectsMalformedToolsAndErrorsWithoutSecrets(t *testing.T) {
	for _, tc := range []struct{ name, s, code string }{
		{"not JSON", "secret-token\n", "malformed_stream"},
		{"missing final", initRecord, "malformed_stream"},
		{"missing init", goodResult, "malformed_stream"},
		{"duplicate final", initRecord + goodResult + goodResult, "malformed_stream"},
		{"builtin availability", `{"type":"system","subtype":"init","tools":["Bash"]}` + "\n", "tools_unsupported"},
		{"mcp availability", `{"type":"system","subtype":"init","mcp_servers":[{"name":"secret-token"}]}` + "\n", "tools_unsupported"},
		{"tool call", initRecord + `{"type":"assistant","message":{"id":"msg_1","model":"claude-configured-a","content":[{"type":"tool_use","name":"Bash","input":{"command":"secret-token"}}]}}` + "\n", "tools_unsupported"},
		{"tool stream", initRecord + `{"type":"stream_event","event":{"type":"content_block_start","content_block":{"type":"tool_use"}}}` + "\n", "tools_unsupported"},
		{"rate", `{"type":"result","is_error":true,"errors":["rate_limit_error secret-token"]}` + "\n", "rate_limited"},
		{"auth", `{"type":"error","error":{"type":"authentication_error","message":"secret-token"}}` + "\n", "external_reauth_required"},
		{"assistant auth", initRecord + `{"type":"assistant","error":"authentication_failed","message":{"model":"<synthetic>","content":[{"type":"text","text":"secret-token"}]}}` + "\n", "external_reauth_required"},
		{"assistant invalid model", initRecord + `{"type":"assistant","error":"model_not_found","message":{"model":"<synthetic>","content":[{"type":"text","text":"secret-token"}]}}` + "\n", "invalid_model"},
		{"rate event", initRecord + `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","utilization":0.9}}` + "\n", "rate_limited"},
		{"model", `{"type":"result","is_error":true,"errors":["invalid model secret-token"]}` + "\n", "invalid_model"},
		{"effort", `{"type":"result","is_error":true,"errors":["unsupported effort secret-token"]}` + "\n", "invalid_effort"},
		{"subscription", `{"type":"result","is_error":true,"errors":["subscription unavailable secret-token"]}` + "\n", "subscription_unavailable"},
		{"process", `{"type":"result","is_error":true,"errors":["secret-token"]}` + "\n", "subprocess_failure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, e := parseStream(strings.NewReader(tc.s), llm.RequestOptions{})
			requireCode(t, e, tc.code)
			if strings.Contains(e.Error(), "secret-token") {
				t.Fatal("error leaked")
			}
		})
	}
}

func TestUsageUnknownZeroAndOverflow(t *testing.T) {
	zero := int64(0)
	u, e := normalizeUsage(&wireUsage{Input: &zero, Cached: &zero, CacheWrite: &zero, Output: &zero})
	if e != nil || len(u.Raw) == 0 || len(u.Unknown) != 1 {
		t.Fatal(u, e)
	}
	u, e = normalizeUsage(nil)
	if e != nil || len(u.Raw) != 0 || len(u.Unknown) != 5 {
		t.Fatal(u, e)
	}
	u, e = normalizeUsage(&wireUsage{})
	if e != nil || len(u.Raw) != 0 || len(u.Unknown) != 5 {
		t.Fatal("empty object became known usage", u, e)
	}
	ten := int64(10)
	u, e = normalizeUsage(&wireUsage{Input: &ten, Output: &ten})
	if e != nil || u.InputTokens != 10 || !slices.Contains(u.Unknown, llm.UsageInput) {
		t.Fatal(u, e)
	}
	tooBig, one, negative := int64(math.MaxInt64), int64(1), int64(-1)
	for _, w := range []*wireUsage{{Input: &tooBig, Cached: &one}, {Output: &negative}} {
		_, e = normalizeUsage(w)
		requireCode(t, e, "malformed_stream")
	}
}

func TestStreamMultipleContentBlocksAndAllowedRateMetadata(t *testing.T) {
	s := initRecord + `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","utilization":0.99,"resetsAt":123}}` + "\n"
	for _, content := range []string{`{"type":"text","text":"first"}`, `{"type":"thinking","thinking":"private-secret"}`, `{"type":"text","text":"second"}`} {
		s += `{"type":"assistant","message":{"id":"msg_multi","model":"claude-configured-a","content":[` + content + `]}}` + "\n"
	}
	s += goodResult
	r, e := parseStream(strings.NewReader(s), llm.RequestOptions{})
	if e != nil || r.Output[0].Data.(llm.Message).Text != "firstsecond" {
		t.Fatal("lost a content block", r, e)
	}
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), "private-secret") || strings.Contains(string(b), "utilization") {
		t.Fatal("private/quota metadata leaked")
	}
}

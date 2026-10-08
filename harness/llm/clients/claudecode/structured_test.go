//go:build linux || darwin

package claudecode

import (
	"bufio"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

func structuredClient(t *testing.T) (*Client, *testclaude.Fixture, llm.Request, llm.RequestOptions) {
	c, f, req := bridgeClient(t)
	c.config.ToolBridge.Mode = BridgeModeStructured
	opt := llm.RequestOptions{Tools: bridgeReply, ActionScope: "canonical-fixture-input", RefreshContext: func(context.Context, int64) (llm.Request, int64, error) { return req, 24576, nil }}
	return c, f, req, opt
}

func TestStructuredOfficialControlSchemaAndHostReceipt(t *testing.T) {
	c, f, req, opt := structuredClient(t)
	f.Set(t, testclaude.Config{Subscription: "team", BridgeManagedPermissionsOnly: true, BridgeSteps: []testclaude.BridgeStep{{Name: "read", Arguments: `{"path":"fixture.go"}`, Contains: "canonical file receipt", Duplicate: true}}})
	var calls, refresh atomic.Int32
	opt.Tools = func(ctx context.Context, response llm.Response) ([]llm.ToolOutcome, error) {
		calls.Add(1)
		if len(response.Output) != 1 || response.Output[0].Type != llm.ItemToolCall {
			t.Fatal("protocol JSON leaked to public response")
		}
		return bridgeReply(ctx, response)
	}
	opt.RefreshContext = func(context.Context, int64) (llm.Request, int64, error) { refresh.Add(1); return req, 24576, nil }
	if err := c.ProbeToolBridge(t.Context(), req.Tools); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls(t)) != 0 || len(f.StructuredProbes(t)) != 1 {
		t.Fatal("schema probe sent inference")
	}
	r, err := c.Respond(t.Context(), req, opt)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || refresh.Load() != 3 || len(f.Calls(t)) != 3 || r.Output[0].Data.(llm.Message).Text != "Bridge fixture completed" {
		t.Fatal("lost Action/Final loop or duplicated execution", calls.Load(), refresh.Load(), len(f.Calls(t)))
	}
	for _, call := range f.Calls(t) {
		argument(t, call.Arguments, "--tools", "")
		argument(t, call.Arguments, "--allowedTools", "StructuredOutput")
		argument(t, call.Arguments, "--disallowedTools", structuredDeny)
		argument(t, call.Arguments, "--mcp-config", `{"mcpServers":{}}`)
		argument(t, call.Arguments, "--max-turns", "5")
		for _, flag := range []string{"--safe-mode", "--restricted", "--strict-mcp-config", "--no-session-persistence", "--no-chrome"} {
			if !slices.Contains(call.Arguments, flag) {
				t.Fatal("isolation removed", flag)
			}
		}
		if call.Directory == f.Directory || strings.Contains(strings.Join(call.Arguments, " "), "prompt-sensitive") || strings.Contains(call.Schema, "mcp__unreal") || !strings.Contains(call.Schema, `"enum":["read"]`) || strings.Contains(call.Schema, `"oneOf"`) {
			t.Fatal("schema/transport isolation violated")
		}
		for _, env := range call.Environment {
			if strings.HasPrefix(env, "ANTHROPIC_API_KEY=") || strings.HasPrefix(env, "OTEL_") || strings.HasPrefix(env, "OPENAI_") || strings.HasPrefix(env, "GH_TOKEN=") || strings.HasPrefix(env, "CLAUDE_CODE_SIMPLE=") {
				t.Fatal("parent environment inherited")
			}
		}
	}
	encoded, _ := json.Marshal(r)
	for _, bad := range []string{"private-", "serializer-only", "StructuredOutput", "account-sensitive"} {
		if strings.Contains(string(encoded), bad) {
			t.Fatal("private serializer metadata leaked")
		}
	}
}

func TestStructuredSchemaAndInvalidProtocolFailClosed(t *testing.T) {
	valid := `{"type":"action","action":{"id":"a","tool":"read","arguments":{"path":"x"}}}`
	for _, test := range []struct{ raw, code string }{
		{`{`, "structured_protocol_invalid"},
		{`{"type":"action","action":{"id":"a","tool":"read","arguments":{"path":true}}}`, "bridge_arguments_invalid"},
		{`{"type":"action","action":{"id":"a","tool":"read"}}`, "bridge_arguments_invalid"},
		{`{"type":"action","action":{"id":"a","tool":"does-not-exist","arguments":{}}}`, "bridge_unknown_tool"},
		{`{"type":"action","action":{"id":"a","tool":"read","arguments":{"path":"x"}},"final":{"message":"done"}}`, "structured_protocol_invalid"},
		{`{"type":"final"}`, "structured_protocol_invalid"},
		{`{"type":"action"}`, "structured_protocol_invalid"},
		{`{"type":"action","type":"final","final":{"message":"done"}}`, "structured_protocol_invalid"},
		{`{"type":"final","final":{"message":"` + strings.Repeat("x", structuredResponseBytes) + `"}}`, "structured_response_too_large"},
	} {
		t.Run(test.code+"/"+test.raw[:min(35, len(test.raw))], func(t *testing.T) {
			c, f, req, opt := structuredClient(t)
			f.Set(t, testclaude.Config{Subscription: "team", StructuredResponses: []string{test.raw}})
			var requests atomic.Int32
			opt.Tools = func(context.Context, llm.Response) ([]llm.ToolOutcome, error) { requests.Add(1); return nil, nil }
			_, err := c.Respond(t.Context(), req, opt)
			requireCode(t, err, test.code)
			if requests.Load() != 0 {
				t.Fatal("malformed output executed work")
			}
		})
	}
	c, f, req, opt := structuredClient(t)
	f.Set(t, testclaude.Config{Subscription: "team", StructuredResponses: []string{valid, strings.Replace(valid, `"x"`, `"different"`, 1)}})
	_, err := c.Respond(t.Context(), req, opt)
	requireCode(t, err, "bridge_duplicate_conflict")
}

func TestStructuredCannotExecuteFreeTextOrClaudeTools(t *testing.T) {
	for _, event := range []string{
		`{"type":"result","subtype":"success","is_error":false,"result":"Please run go test"}`,
		`{"type":"assistant","message":{"id":"bad","model":"claude-configured-a","content":[{"type":"tool_use","id":"bad","name":"Bash","input":{"command":"touch direct-side-effect"}}]}}`,
		`{"type":"assistant","message":{"id":"bad","model":"claude-configured-a","content":[{"type":"tool_use","id":"bad","name":"Read","input":{}}]}}`,
		`{"type":"assistant","message":{"id":"bad","model":"claude-configured-a","content":[{"type":"tool_use","id":"bad","name":"Edit","input":{}}]}}`,
		`{"type":"assistant","message":{"id":"bad","model":"claude-configured-a","content":[{"type":"tool_use","id":"bad","name":"Agent","input":{}}]}}`,
		`{"type":"assistant","message":{"id":"bad","model":"claude-configured-a","content":[{"type":"tool_use","id":"bad","name":"mcp__foreign__read","input":{}}]}}`,
		`{"type":"system","subtype":"task_started","task_id":"private-task"}`,
	} {
		c, f, req, opt := structuredClient(t)
		f.Set(t, testclaude.Config{Subscription: "team", Stream: `{"type":"system","subtype":"init","model":"claude-configured-a","tools":["StructuredOutput"],"mcp_servers":[],"permissionMode":"default"}` + "\n" + event + "\n"})
		var calls atomic.Int32
		opt.Tools = func(context.Context, llm.Response) ([]llm.ToolOutcome, error) { calls.Add(1); return nil, nil }
		_, err := c.Respond(t.Context(), req, opt)
		if err == nil || calls.Load() != 0 {
			t.Fatal("free text/side-effecting Claude tool accepted", err)
		}
	}
}

func TestStructuredPolicyDenialAndFeatureUnavailable(t *testing.T) {
	for _, cfg := range []testclaude.Config{
		{Subscription: "team", StructuredInitializeError: true},
		{Subscription: "team", MissingFlag: "--json-schema"},
		{Subscription: "team", BridgeEvent: `{"type":"system","subtype":"permission_denied","message":"private-token-sensitive","decision_reason":"private-policy-sensitive","decision_reason_type":"rule","session_id":"private-session-sensitive"}`},
	} {
		c, f, req, opt := structuredClient(t)
		f.Set(t, cfg)
		_, err := c.Respond(t.Context(), req, opt)
		requireCode(t, err, "structured_unavailable")
		if strings.Contains(err.Error(), "private-") {
			t.Fatal("denial leaked body")
		}
	}
}

func TestStructuredLaunchAndConfigContract(t *testing.T) {
	if err := validateStructuredLaunch(append(structuredArgs(), "--max-turns", structuredSerializerTurns)); err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"--safe-mode", "--tools", "--strict-mcp-config", "--mcp-config", "--allowedTools", "--disallowedTools"} {
		args := append(structuredArgs(), "--max-turns", structuredSerializerTurns)
		index := slices.Index(args, flag)
		args = slices.Delete(args, index, index+1)
		requireCode(t, validateStructuredLaunch(args), "isolation_contract_invalid")
	}
	for _, cfg := range []ToolBridgeConfig{{Mode: "unknown"}, {Mode: BridgeModeStructured, MaxActionRounds: 129}, {MaxActionRounds: -1}} {
		requireCode(t, cfg.Validate(), "bridge_configuration_invalid")
	}
	if (ToolBridgeConfig{Enabled: true}).ResolvedMode() != BridgeModeMCP || (ToolBridgeConfig{Mode: BridgeModeStructured}).ResolvedMode() != "disabled" || (ToolBridgeConfig{}).limits().MaxActionRounds != 32 {
		t.Fatal("config compatibility/defaults changed")
	}
	// Remote schema references cannot read a local file or send a request.
	_, err := newActionSchema([]llm.Tool{{Type: llm.ToolFunction, Name: "read", Parameters: map[string]any{"$ref": "file:///etc/passwd"}}})
	requireCode(t, err, "bridge_schema_invalid")
	_, err = newActionSchema([]llm.Tool{{Type: llm.ToolFunction, Name: "StructuredOutput", Parameters: map[string]any{"type": "object"}}})
	requireCode(t, err, "bridge_schema_invalid")
	s, err := newActionSchema([]llm.Tool{{Type: llm.ToolFunction, Name: "count", Parameters: map[string]any{"type": "object", "properties": map[string]any{"n": map[string]any{"type": "integer"}}, "required": []string{"n"}}}})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := s.parse(jsontext.Value(`{"type":"action","action":{"id":"a","tool":"count","arguments":{"n":9007199254740992}}}`))
	b, _ := s.parse(jsontext.Value(`{"type":"action","action":{"id":"a","tool":"count","arguments":{"n":9007199254740993}}}`))
	x, _ := actionFingerprint(a)
	y, _ := actionFingerprint(b)
	if x == y {
		t.Fatal("distinct numeric arguments merged")
	}
}

func TestStructuredRoundLimitBudgetAndCancellation(t *testing.T) {
	c, f, req, opt := structuredClient(t)
	c.config.ToolBridge.MaxActionRounds = 1
	f.Set(t, testclaude.Config{Subscription: "team", BridgeSteps: []testclaude.BridgeStep{{Name: "read", Arguments: `{"path":"one"}`}, {Name: "read", Arguments: `{"path":"two"}`}}})
	var calls atomic.Int32
	opt.Tools = func(ctx context.Context, r llm.Response) ([]llm.ToolOutcome, error) {
		calls.Add(1)
		return bridgeReply(ctx, r)
	}
	_, err := c.Respond(t.Context(), req, opt)
	requireCode(t, err, "action_round_limit")
	if calls.Load() != 1 {
		t.Fatal("round limit executed excess work")
	}
	c, f, req, opt = structuredClient(t)
	opt.InputBudget = 128
	_, err = c.Respond(t.Context(), req, opt)
	requireCode(t, err, "bridge_context_budget")
	if len(f.Calls(t)) != 0 {
		t.Fatal("over-budget request reached inference")
	}
	c, f, req, opt = structuredClient(t)
	f.Set(t, testclaude.Config{Subscription: "team", Wait: true, Child: true})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, err := c.Respond(ctx, req, opt); done <- err }()
	deadline := time.Now().Add(10 * time.Second)
	var recorded []testclaude.Call
	for time.Now().Before(deadline) {
		recorded = f.Calls(t)
		if len(recorded) > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if len(recorded) == 0 {
		cancel()
		t.Fatal("fixture not started")
	}
	cancel()
	select {
	case err = <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation stalled")
	}
	waitForProcessGone(t, recorded[0].PID)
	waitForProcessGone(t, recorded[0].ChildPID)
}

func waitForProcessGone(t *testing.T, pid int) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("process not cleaned", pid)
}

func TestStructuredResultBoundAndSecretExclusion(t *testing.T) {
	receipt := llm.ToolOutcome{Result: llm.ToolResult{CallID: "receipt", Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: strings.Repeat("日本語", 20000)}}}}
	_, input, err := structuredPrompt(textRequest().Input, &receipt, 1024)
	if err != nil || !strings.Contains(input, "truncated") || len(input) > 4096 {
		t.Fatal("receipt unbounded or silently truncated", err, len(input))
	}
	receipt.Result.Output[0].Value = "ANTHROPIC_API_KEY=sk-ant-test-secret-sensitive"
	_, input, err = structuredPrompt(textRequest().Input, &receipt, 1024)
	if err != nil || strings.Contains(input, "sk-ant-test-secret-sensitive") {
		t.Fatal("secret receipt leaked", err)
	}
}

func TestStructuredSchemaSnapshotAndResultIntegrity(t *testing.T) {
	parameters := map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}, "required": []string{"path"}}
	tools := []llm.Tool{{Type: llm.ToolFunction, Name: "read", Parameters: parameters}}
	schema, err := newActionSchema(tools)
	if err != nil {
		t.Fatal(err)
	}
	original := string(schema.document)
	parameters["properties"].(map[string]any)["path"] = map[string]any{"type": "boolean"}
	updated, err := newActionSchema(tools)
	if err != nil || sameSchema(schema, updated) || string(schema.document) != original {
		t.Fatal("schema changed after request snapshot", err)
	}
	const init = `{"type":"system","subtype":"init","model":"synthetic-model","tools":["StructuredOutput"],"mcp_servers":[],"permissionMode":"default"}` + "\n"
	const final = `{"type":"result","subtype":"success","is_error":false,"structured_output":{"type":"final","final":{"message":"completed"}}}` + "\n"
	for _, record := range safeMetadata {
		stream := init + metadataRecord(record.name, record.body) + final
		proposal, response, err := readStructuredStream(bufio.NewScanner(strings.NewReader(stream)), schema, nil, nil)
		if err != nil || proposal.Type != "final" || proposal.Final.Message != "completed" || response.Model != "synthetic-model" {
			t.Fatal("lost validated official system metadata", record.name, err)
		}
	}
	for _, stream := range []string{
		init + `{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"tool_use"}}}` + "\n" + final,
		init + metadataRecord("informational", `"content":"private-sensitive","level":"notice","tool_use_id":"private-tool"`) + final,
		init + metadataRecord("thinking_tokens", `"estimated_tokens":1,"estimated_tokens_delta":1,"mcp_server_name":"private-server"`) + final,
		init + final + `{"type":"assistant","message":{"id":"bad","model":"synthetic-model","content":[{"type":"tool_use","id":"bad","name":"Bash","input":{}}]}}` + "\n",
	} {
		_, _, err := readStructuredStream(bufio.NewScanner(strings.NewReader(stream)), schema, nil, nil)
		if err == nil || strings.Contains(err.Error(), "private-sensitive") {
			t.Fatal("execution marker hidden", err)
		}
	}
	// The size bound applies to the aggregate serializer input, not just each
	// independently small streaming fragment.
	fragment, _ := json.Marshal(map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": strings.Repeat("x", 32768)}}})
	stream := init + `{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"serializer","name":"StructuredOutput"}}}` + "\n" + strings.Repeat(string(fragment)+"\n", 33) + final
	_, _, err = readStructuredStream(bufio.NewScanner(strings.NewReader(stream)), schema, nil, nil)
	requireCode(t, err, "structured_response_too_large")
	requireStructuredStage(t, err, "structured_response_oversized")
}

func TestStructuredProviderCrashAndGenerationTimeoutDoNotExecute(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		cfg        testclaude.Config
	}{
		{"crash", "subprocess_failure", testclaude.Config{Subscription: "team", Exit: 31, BridgeSteps: []testclaude.BridgeStep{{Name: "read", Arguments: `{"path":"fixture.go"}`}}}},
		{"timeout", "structured_generation_timeout", testclaude.Config{Subscription: "team", Wait: true, Child: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, f, req, opt := structuredClient(t)
			c.config.ToolBridge.TimeoutMillis = 1000
			f.Set(t, tc.cfg)
			var calls atomic.Int32
			opt.Tools = func(ctx context.Context, r llm.Response) ([]llm.ToolOutcome, error) {
				calls.Add(1)
				return bridgeReply(ctx, r)
			}
			_, err := c.Respond(t.Context(), req, opt)
			requireCode(t, err, tc.code)
			if calls.Load() != 0 || len(f.Calls(t)) != 1 {
				t.Fatal("failed provider generation executed work", calls.Load())
			}
			call := f.Calls(t)[0]
			waitForProcessGone(t, call.PID)
			if call.ChildPID != 0 {
				waitForProcessGone(t, call.ChildPID)
			}
		})
	}
}

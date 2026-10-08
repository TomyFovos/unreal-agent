//go:build linux || darwin

package claudecode

import (
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

func bridgeClient(t *testing.T) (*Client, *testclaude.Fixture, llm.Request) {
	t.Helper()
	c, f := fakeClient(t)
	c.config.ManagedPolicyMode = ManagedPolicyTrust
	c.config.ToolBridge = ToolBridgeConfig{Enabled: true}
	r := textRequest()
	r.Tools = []llm.Tool{{Type: llm.ToolFunction, Name: "read", Parameters: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}, "required": []string{"path"}}}}
	return c, f, r
}
func bridgeReply(_ context.Context, r llm.Response) ([]llm.ToolOutcome, error) {
	call := r.Output[len(r.Output)-1].Data.(llm.ToolCall)
	return []llm.ToolOutcome{{Result: llm.ToolResult{CallID: call.CallID, Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "canonical file receipt\n日本語 👩🏽‍💻"}}}}}, nil
}

func TestBridgeSDKToolCallbackContinuationIsolationAndDuplicate(t *testing.T) {
	c, f, req := bridgeClient(t)
	f.Set(t, testclaude.Config{Subscription: "team", BridgeSteps: []testclaude.BridgeStep{{Name: "read", Arguments: `{"path":"fixture.go"}`, Contains: "canonical file receipt", Duplicate: true, DuplicateEnvelope: true}}})
	var called atomic.Int32
	r, err := c.Respond(t.Context(), req, llm.RequestOptions{Tools: func(ctx context.Context, response llm.Response) ([]llm.ToolOutcome, error) {
		called.Add(1)
		return bridgeReply(ctx, response)
	}})
	if err != nil {
		t.Fatal(err)
	}
	if called.Load() != 1 || r.Model != "claude-configured-a" || r.Output[0].Data.(llm.Message).Text != "Bridge fixture completed" || r.Usage.InputTokens != 10 || r.Usage.OutputTokens != 2 {
		t.Fatal("lost official callback/result continuation or duplicated execution", called.Load(), r)
	}
	calls := f.Calls(t)
	if len(calls) != 1 {
		t.Fatal("bridge launched multiple inference processes")
	}
	call := calls[0]
	for _, flag := range []string{"--safe-mode", "--restricted", "--strict-mcp-config", "--no-session-persistence", "--no-chrome", "--disable-slash-commands"} {
		if !slices.Contains(call.Arguments, flag) {
			t.Fatal("missing isolation", flag)
		}
	}
	argument(t, call.Arguments, "--tools", "")
	argument(t, call.Arguments, "--disallowedTools", bridgeBuiltinDeny)
	argument(t, call.Arguments, "--allowedTools", "mcp__unreal__unreal_read")
	argument(t, call.Arguments, "--mcp-config", bridgeMCP)
	argument(t, call.Arguments, "--input-format", "stream-json")
	argument(t, call.Arguments, "--max-turns", "33")
	argument(t, call.Arguments, "--permission-prompts", "none")
	if strings.Contains(strings.Join(call.Arguments, " "), "prompt-sensitive") {
		t.Fatal("prompt in argv")
	}
	for _, env := range call.Environment {
		if strings.HasPrefix(env, "ANTHROPIC_API_KEY=") || strings.HasPrefix(env, "OTEL_") || strings.HasPrefix(env, "OPENAI_") || strings.HasPrefix(env, "GH_TOKEN=") || strings.HasPrefix(env, "CLAUDE_CODE_SIMPLE=") {
			t.Fatal("parent credential/routing environment inherited")
		}
	}
	b, _ := json.Marshal(r)
	for _, bad := range []string{"private-reasoning-sensitive", "token-sensitive", "account-sensitive", "private-session-sensitive", "private-cli-result-sensitive"} {
		if strings.Contains(string(b), bad) {
			t.Fatal("private state leaked")
		}
	}
}

func TestBridgeOnlySDKProvenanceCanReachPrompt(t *testing.T) {
	for _, cfg := range []testclaude.Config{{Subscription: "team", BridgeSource: "managed"}, {Subscription: "team", BridgeSource: "plugin"}, {Subscription: "team", BridgeExtraServer: true}} {
		c, f, req := bridgeClient(t)
		f.Set(t, cfg)
		_, err := c.Respond(t.Context(), req, llm.RequestOptions{Tools: bridgeReply})
		requireCode(t, err, "tools_unsupported")
		if len(f.Calls(t)) != 0 {
			t.Fatal("untrusted MCP crossed pre-inference attestation")
		}
	}
	c, f, req := bridgeClient(t)
	f.Set(t, testclaude.Config{Subscription: "team"})
	if err := c.ProbeToolBridge(t.Context(), req.Tools); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls(t)) != 0 || len(f.CatalogCalls(t)) != 0 {
		t.Fatal("SDK probe sent user/inference or reused catalog stdout")
	}
}

func TestBridgeLaunchContractFailsClosed(t *testing.T) {
	_, _, req := bridgeClient(t)
	tools, err := bridgeTools(req.Tools)
	if err != nil {
		t.Fatal(err)
	}
	args := bridgeArgs(tools)
	if err = validateBridgeLaunch(args, tools); err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"--safe-mode", "--restricted", "--tools", "--disallowedTools", "--mcp-config", "--strict-mcp-config", "--allowedTools"} {
		x := slices.Clone(args)
		i := slices.Index(x, flag)
		x = append(x[:i], x[i+1:]...)
		requireCode(t, validateBridgeLaunch(x, tools), "isolation_contract_invalid")
	}
	x := slices.Clone(args)
	i := slices.Index(x, "--tools")
	x[i+1] = "Bash,Read,Edit"
	requireCode(t, validateBridgeLaunch(x, tools), "isolation_contract_invalid")
}

func TestBridgeExecutionViolationsRemainFatal(t *testing.T) {
	for _, event := range []string{
		`{"type":"system","subtype":"init","model":"claude-configured-a","tools":["Bash"],"mcp_servers":[],"permissionMode":"default"}`,
		`{"type":"system","subtype":"init","model":"claude-configured-a","tools":["Read"],"mcp_servers":[],"permissionMode":"default"}`,
		`{"type":"system","subtype":"init","model":"claude-configured-a","tools":["Edit"],"mcp_servers":[],"permissionMode":"default"}`,
		`{"type":"system","subtype":"init","model":"claude-configured-a","tools":[],"mcp_servers":[{"name":"arbitrary","status":"connected"}],"permissionMode":"default"}`,
		`{"type":"system","subtype":"task_started","task_id":"private-task-sensitive"}`,
	} {
		c, f, req := bridgeClient(t)
		f.Set(t, testclaude.Config{Subscription: "team", BridgeEvent: event})
		_, err := c.Respond(t.Context(), req, llm.RequestOptions{Tools: func(context.Context, llm.Response) ([]llm.ToolOutcome, error) {
			t.Error("execution violation called Unreal")
			return nil, nil
		}})
		if err == nil || strings.Contains(err.Error(), "private-task-sensitive") {
			t.Fatal("unsafe execution accepted or leaked", err)
		}
	}
}

func TestBridgeLimitsFailureAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		steps []testclaude.BridgeStep
		cfg   ToolBridgeConfig
		code  string
	}{
		{"round limit", []testclaude.BridgeStep{{Name: "read", Arguments: `{"path":"a"}`}, {Name: "read", Arguments: `{"path":"b"}`}}, ToolBridgeConfig{Enabled: true, MaxToolRounds: 1}, "bridge_round_limit"},
		{"unknown", []testclaude.BridgeStep{{Name: "unregistered", Arguments: `{}`}}, ToolBridgeConfig{Enabled: true}, "tools_unsupported"},
		{"malformed args", []testclaude.BridgeStep{{Name: "read", Arguments: `null`}}, ToolBridgeConfig{Enabled: true}, "bridge_arguments_invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, f, req := bridgeClient(t)
			c.config.ToolBridge = tc.cfg
			f.Set(t, testclaude.Config{Subscription: "team", BridgeSteps: tc.steps})
			_, err := c.Respond(t.Context(), req, llm.RequestOptions{Tools: bridgeReply})
			requireCode(t, err, tc.code)
		})
	}
	c, f, req := bridgeClient(t)
	c.config.ToolBridge.TimeoutMillis = 1000
	f.Set(t, testclaude.Config{Subscription: "team", BridgeSteps: []testclaude.BridgeStep{{Name: "read", Arguments: `{"path":"a"}`}}})
	_, err := c.Respond(t.Context(), req, llm.RequestOptions{Tools: func(ctx context.Context, _ llm.Response) ([]llm.ToolOutcome, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}})
	requireCode(t, err, "bridge_tool_timeout")
	ctx, cancel := context.WithCancel(t.Context())
	called := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		_, err := c.Respond(ctx, req, llm.RequestOptions{Tools: func(ctx context.Context, _ llm.Response) ([]llm.ToolOutcome, error) {
			called <- struct{}{}
			<-ctx.Done()
			return nil, ctx.Err()
		}})
		done <- err
	}()
	select {
	case <-called:
	case <-time.After(3 * time.Second):
		t.Fatal("callback did not start")
	}
	cancel()
	select {
	case err = <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancel leaked process or pipe goroutine")
	}
	for _, call := range f.Calls(t) {
		if _, err := os.Stat(call.Directory); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("request directory leaked")
		}
	}
}

func TestBridgeDisabledSchemaProtectionAndConfig(t *testing.T) {
	c, _, req := bridgeClient(t)
	c.config.ToolBridge.Enabled = false
	_, err := c.Respond(t.Context(), req, llm.RequestOptions{Tools: bridgeReply})
	requireCode(t, err, "tools_unsupported")
	for _, cfg := range []ToolBridgeConfig{{Enabled: true, MaxToolRounds: -1}, {Enabled: true, MaxToolRounds: 129}, {Enabled: true, ResultBytes: 100}, {Enabled: true, TimeoutMillis: 1}} {
		_, err := NewClient(Config{ToolBridge: cfg})
		requireCode(t, err, "bridge_configuration_invalid")
	}
	if (ToolBridgeConfig{}).Enabled {
		t.Fatal("silent opt-in")
	}
}

func TestBridgeBoundsProtectedAndLargeCanonicalReceipts(t *testing.T) {
	for _, tc := range []struct {
		name, value, contains string
		failed                bool
	}{
		{"protected", "authorization: Bearer PRIVATE_TOKEN_SHOULD_NOT_RETURN", "receipt withheld", true},
		{"large", strings.Repeat("日本語👩🏽‍💻\n", 5000), "full receipt retained in canonical history", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, f, req := bridgeClient(t)
			c.config.ToolBridge.ResultBytes = 1024
			f.Set(t, testclaude.Config{Subscription: "team", BridgeSteps: []testclaude.BridgeStep{{Name: "read", Arguments: `{"path":"file"}`, Contains: tc.contains, Error: tc.failed}}})
			_, err := c.Respond(t.Context(), req, llm.RequestOptions{Tools: func(_ context.Context, response llm.Response) ([]llm.ToolOutcome, error) {
				call := response.Output[len(response.Output)-1].Data.(llm.ToolCall)
				return []llm.ToolOutcome{{Result: llm.ToolResult{CallID: call.CallID, Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: tc.value}}}}}, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			if tc.name == "large" {
				bounded := boundedBridgeText(tc.value, 1024)
				if len(bounded) > 1024 || !utf8.ValidString(bounded) || !strings.Contains(bounded, "original bytes=") {
					t.Fatal("silent or unsafe truncation")
				}
			}
		})
	}
}

func TestBridgeCancelReapsGroupAndCrashIsTyped(t *testing.T) {
	c, f, req := bridgeClient(t)
	f.Set(t, testclaude.Config{Subscription: "team", Wait: true, Child: true})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, err := c.Respond(ctx, req, llm.RequestOptions{Tools: bridgeReply}); done <- err }()
	deadline := time.Now().Add(3 * time.Second)
	for len(f.Calls(t)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("bridge process did not start")
		}
		time.Sleep(time.Millisecond)
	}
	call := f.Calls(t)[0]
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("bridge cancellation leaked process/pipe worker")
	}
	for _, pid := range []int{call.PID, call.ChildPID} {
		if pid <= 0 || !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			t.Fatal("bridge process group not reaped", pid)
		}
	}
	f.Set(t, testclaude.Config{Subscription: "team", Exit: 7})
	_, err := c.Respond(t.Context(), req, llm.RequestOptions{Tools: bridgeReply})
	requireCode(t, err, "subprocess_failure")
}

func TestBridgeRejectsUnownedExecutionAndMalformedAfterValidInit(t *testing.T) {
	const init = `{"type":"system","subtype":"init","model":"claude-configured-a","tools":[],"mcp_servers":[],"permissionMode":"default"}` + "\n"
	for _, event := range []string{
		`{"type":"assistant","message":{"id":"m","model":"claude-configured-a","content":[{"type":"tool_use","id":"u","name":"Bash","input":{}}]}}`,
		`{"type":"assistant","message":{"id":"m","model":"claude-configured-a","content":[{"type":"server_tool_use","id":"u","name":"web_search","input":{}}]}}`,
		`{"type":"assistant","message":{"id":"m","model":"claude-configured-a","content":[{"type":"tool_use","id":"u","name":"mcp__foreign__read","input":{}}]}}`,
		`{"type":"system","subtype":"task_started","task_id":"private-sensitive"}`,
		`{"type":"tool_progress","tool_use_id":"unknown","tool_name":"Bash"}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"{}"}}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"unknown"}]}}`,
		`{"type":"control_request","request_id":"bad","request":{"subtype":"mcp_message","server_name":"foreign","message":{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read","arguments":{}}}}}`,
		`{"type":"system","subtype":"status","status":42}`,
	} {
		c, f, req := bridgeClient(t)
		f.Set(t, testclaude.Config{Subscription: "team", BridgeEvent: init + event})
		_, err := c.Respond(t.Context(), req, llm.RequestOptions{Tools: func(context.Context, llm.Response) ([]llm.ToolOutcome, error) {
			t.Error("unowned execution reached host")
			return nil, nil
		}})
		if err == nil || strings.Contains(err.Error(), "private-sensitive") {
			t.Fatal("unowned execution/malformed record accepted or leaked", err)
		}
	}
}

func TestBridgeRPCIdentityAndBudgetAreClosed(t *testing.T) {
	var a, b bridgeWire
	json.Unmarshal([]byte(`{"type":"control_request","request_id":"a","request":{"subtype":"mcp_message","server_name":"unreal","message":{"jsonrpc":"2.0","id":"rpc","method":"tools/call","params":{"name":"unreal_read","arguments":{"path":"x"}}}}}`), &a)
	b = a
	b.ID = "b"
	if !sameBridgeRequest(a, b) {
		t.Fatal("transport retry lost its RPC identity")
	}
	b.Request.Message.Params.Arguments = []byte(`{"path":"different"}`)
	if sameBridgeRequest(a, b) {
		t.Fatal("conflicting side effect accepted under same identity")
	}
	for _, id := range []string{`null`, `[]`, `{}`, `""`, `1.5`, `9223372036854775808`} {
		_, err := bridgeRPCIdentity([]byte(id))
		requireCode(t, err, "bridge_protocol_failure")
	}
	first, firstErr := bridgeRPCIdentity([]byte(`9007199254740992`))
	if firstErr != nil {
		t.Fatal(firstErr)
	}
	second, secondErr := bridgeRPCIdentity([]byte(`9007199254740993`))
	if secondErr != nil || first == second {
		t.Fatal("different MCP request identities merged through floating point", secondErr)
	}
	c, f, req := bridgeClient(t)
	_, err := c.Respond(t.Context(), req, llm.RequestOptions{Tools: bridgeReply, InputBudget: 64})
	requireCode(t, err, "bridge_context_budget")
	if len(f.Calls(t)) != 0 {
		t.Fatal("invalid budget reached inference")
	}
	if image, err := bridgeImage("data:image/png;base64,YQ==", 1024); err != nil || image["mimeType"] != "image/png" {
		t.Fatal("canonical image receipt mapping", err)
	}
	if image, err := bridgeImage("data:image/png;base64,YQ==", 1); err != nil || image != nil {
		t.Fatal("image exceeded result bound")
	}
	_, err = bridgeImage("https://remote.invalid/image", 1024)
	requireCode(t, err, "bridge_receipt_invalid")
}

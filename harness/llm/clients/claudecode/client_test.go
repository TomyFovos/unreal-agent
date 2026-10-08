//go:build linux || darwin

package claudecode

import (
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

func fakeClient(t *testing.T) (*Client, *testclaude.Fixture) {
	t.Helper()
	f := testclaude.New(t)
	c, e := NewClient(Config{Binary: f.Binary, Models: []modelcatalog.Model{{ID: "claude-configured-a", Efforts: []llm.ReasoningEffort{"low", "medium"}}, {ID: "claude-configured-b", Efforts: []llm.ReasoningEffort{"high"}}}, Getenv: f.Getenv})
	if e != nil {
		t.Fatal(e)
	}
	return c, f
}
func textRequest() llm.Request {
	return llm.Request{Model: llm.Model{ID: "claude-configured-a", ReasoningEffort: "medium"}, Input: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleSystem, Text: "system-prompt-sensitive"}}, {Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "user-prompt-sensitive"}}}}
}
func argument(t *testing.T, args []string, name, want string) {
	t.Helper()
	i := slices.Index(args, name)
	if i < 0 || i+1 >= len(args) || args[i+1] != want {
		t.Fatalf("missing required %s=%q", name, want)
	}
}

func TestDirectInvocationSubscriptionIsolationAndStdin(t *testing.T) {
	for _, mode := range []ManagedPolicyMode{ManagedPolicyReject, ManagedPolicyTrust} {
		t.Run(string(mode), func(t *testing.T) { testDirectInvocation(t, mode) })
	}
}

func testDirectInvocation(t *testing.T, mode ManagedPolicyMode) {
	t.Helper()
	c, f := fakeClient(t)
	c.config.ManagedPolicyMode = mode
	if mode == ManagedPolicyTrust {
		f.Set(t, testclaude.Config{Subscription: "team", ManagedTelemetry: true})
	}
	// Unrelated secret-bearing parent variables are dropped. Routing variables
	// are left unset here and covered by explicit rejection tests below.
	c.config.Getenv = func(k string) string {
		switch k {
		case "HOME":
			return f.Home
		case "CLAUDE_CODE_SIMPLE":
			return "1" // Simple/bare mode would hide stored subscription OAuth.
		case "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "OPENAI_API_KEY", "OPENAI_CODEX_ACCESS_TOKEN", "NODE_OPTIONS", "LD_PRELOAD", "HTTPS_PROXY", "CLAUDE_CODE_ENABLE_TELEMETRY", "OTEL_LOGS_EXPORTER", "OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_HEADERS", "OTEL_LOG_TOOL_DETAILS":
			return "parent-environment-sensitive"
		}
		return ""
	}
	var progress []llm.Progress
	r, err := c.Respond(t.Context(), textRequest(), llm.RequestOptions{Progress: func(p llm.Progress) { progress = append(progress, p) }})
	if err != nil {
		t.Fatal(err)
	}
	if len(progress) < 2 || !progress[0].Reset || progress[1].Delta != "fake reply" || r.Output[0].Data.(llm.Message).Text != "fake reply" {
		t.Fatal("streaming text missing")
	}
	calls := f.Calls(t)
	if len(calls) != 1 {
		t.Fatal("unexpected model requests", len(calls))
	}
	call := calls[0]
	for _, flag := range []string{"-p", "--safe-mode", "--restricted", "--no-session-persistence", "--strict-mcp-config", "--disable-slash-commands", "--no-chrome", "--verbose", "--include-partial-messages"} {
		if !slices.Contains(call.Arguments, flag) {
			t.Fatalf("isolation flag missing: %s", flag)
		}
	}
	argument(t, call.Arguments, "--tools", "")
	argument(t, call.Arguments, "--disallowedTools", "*")
	argument(t, call.Arguments, "--mcp-config", `{"mcpServers":{}}`)
	argument(t, call.Arguments, "--setting-sources", "")
	argument(t, call.Arguments, "--settings", settings)
	argument(t, call.Arguments, "--model", "claude-configured-a")
	argument(t, call.Arguments, "--effort", "medium")
	argument(t, call.Arguments, "--permission-prompts", "none")
	argument(t, call.Arguments, "--max-turns", "1")
	argument(t, call.Arguments, "--input-format", "text")
	argument(t, call.Arguments, "--output-format", "stream-json")
	for _, flag := range []string{"--bare", "--continue", "--resume", "--fallback-model", "--dangerously-skip-permissions", "--permission-prompt-tool"} {
		if slices.Contains(call.Arguments, flag) {
			t.Fatalf("forbidden flag %s", flag)
		}
	}
	if call.Input != "user-prompt-sensitive" || call.System != "system-prompt-sensitive" {
		t.Fatal("prompt transport lost context")
	}
	argv := strings.Join(call.Arguments, " ")
	for _, secret := range []string{"user-prompt-sensitive", "system-prompt-sensitive", "parent-environment-sensitive"} {
		if strings.Contains(argv, secret) {
			t.Fatal("sensitive argv")
		}
	}
	for _, env := range call.Environment {
		for _, bad := range []string{"CLAUDE_CODE_SIMPLE=", "ANTHROPIC_API_KEY=", "AUTH_TOKEN=", "OAUTH_TOKEN=", "OPENAI_", "NODE_OPTIONS=", "LD_PRELOAD=", "HTTPS_PROXY=", "parent-environment-sensitive"} {
			if strings.Contains(env, bad) {
				t.Fatal("unsafe environment inheritance")
			}
		}
	}
	for _, blocker := range []string{"DISABLE_TELEMETRY=1", "DISABLE_ERROR_REPORTING=1", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1"} {
		if slices.Contains(call.Environment, blocker) != (mode == ManagedPolicyReject) {
			t.Fatal("wrong telemetry boundary", blocker)
		}
	}
	if mode == ManagedPolicyTrust {
		for _, managed := range []string{"CLAUDE_CODE_ENABLE_TELEMETRY=1", "OTEL_LOGS_EXPORTER=otlp", "OTEL_EXPORTER_OTLP_ENDPOINT=https://managed-telemetry-sensitive.invalid", "OTEL_LOG_TOOL_DETAILS=1"} {
			if !slices.Contains(call.Environment, managed) {
				t.Fatal("CLI-managed telemetry was suppressed")
			}
		}
	}
	if _, e := os.Stat(call.Directory); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("private request directory leaked", e)
	}
	encoded, _ := json.Marshal(r)
	for _, secret := range []string{"plugin-catalog-sensitive", "version-catalog-sensitive", "stderr-sensitive", "token-sensitive", "private-api-key-sensitive", "private-reasoning-sensitive", "private-signature-sensitive", "account-sensitive", "claude-internal-session-sensitive", "managed-telemetry-sensitive", "managed-telemetry-credential-sensitive"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatal("private output leaked")
		}
	}
}

func TestBinaryDiscoveryAndPreflightFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config testclaude.Config
		code   string
	}{
		{"version", testclaude.Config{Version: "2.1.284"}, "unsupported_version"},
		{"unknown major", testclaude.Config{Version: "3.0.0"}, "unsupported_version"},
		{"missing isolation", testclaude.Config{MissingFlag: "--safe-mode"}, "unsupported_version"},
		{"unsigned", testclaude.Config{SignedOut: true}, "external_reauth_required"},
		{"API login", testclaude.Config{AuthMethod: "api_key"}, "subscription_unavailable"},
		{"API helper", testclaude.Config{AuthMethod: "api_key_helper"}, "subscription_unavailable"},
		{"inline OAuth", testclaude.Config{AuthMethod: "oauth_token"}, "subscription_unavailable"},
		{"third party", testclaude.Config{APIProvider: "thirdParty"}, "subscription_unavailable"},
		{"Team managed", testclaude.Config{Subscription: "team"}, "policy_isolation_unavailable"},
		{"Enterprise managed", testclaude.Config{Subscription: "enterprise"}, "policy_isolation_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, f := fakeClient(t)
			f.Set(t, tc.config)
			e := c.Probe(t.Context())
			requireCode(t, e, tc.code)
			if len(f.Calls(t)) != 0 {
				t.Fatal("preflight sent inference")
			}
			if tc.code == "external_reauth_required" && !credential.IsCode(e, tc.code) {
				t.Fatal("typed external credential semantics lost")
			}
		})
	}
	c, f := fakeClient(t)
	c.config.Binary = "/nonexistent/unreal-test-claude"
	requireCode(t, c.Probe(t.Context()), "binary_not_found")
	c.config.Binary = "claude"
	t.Setenv("PATH", f.Directory)
	if err := c.Probe(t.Context()); err != nil {
		t.Fatal("PATH discovery failed", err)
	}
	if len(f.Calls(t)) != 0 {
		t.Fatal("probe sent a model request")
	}
}

func TestAlternativeBillingIsRejectedAndPolicyIsNotRead(t *testing.T) {
	for _, key := range []string{"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY", "CLAUDE_CODE_USE_MANTLE", "CLAUDE_CODE_USE_ANTHROPIC_AWS", "ANTHROPIC_BASE_URL", "ANTHROPIC_CUSTOM_HEADERS", "ANTHROPIC_PROFILE"} {
		t.Run(key, func(t *testing.T) {
			c, f := fakeClient(t)
			c.config.Getenv = func(k string) string {
				if k == "HOME" {
					return f.Home
				}
				if k == key {
					return "sensitive-provider-override"
				}
				return ""
			}
			e := c.Probe(t.Context())
			requireCode(t, e, "subscription_mode_conflict")
			if strings.Contains(e.Error(), "sensitive-provider-override") {
				t.Fatal("routing secret leaked")
			}
			if len(f.Calls(t)) > 0 {
				t.Fatal("conflict sent inference")
			}
		})
	}
	c, f := fakeClient(t)
	dir := filepath.Join(f.Home, ".claude")
	if e := os.Mkdir(dir, 0700); e != nil {
		t.Fatal(e)
	}
	// A FIFO would block any accidental attempt to read the policy contents.
	if e := syscall.Mkfifo(filepath.Join(dir, "remote-settings.json"), 0600); e != nil {
		t.Fatal(e)
	}
	requireCode(t, c.Probe(t.Context()), "policy_isolation_unavailable")
}

func TestEachRequestUsesCLIAuthAndModelSpecificEffort(t *testing.T) {
	c, f := fakeClient(t)
	r := textRequest()
	if _, e := c.Respond(t.Context(), r, llm.RequestOptions{}); e != nil {
		t.Fatal(e)
	}
	f.Set(t, testclaude.Config{SignedOut: true})
	_, e := c.Respond(t.Context(), r, llm.RequestOptions{})
	requireCode(t, e, "external_reauth_required")
	if len(f.Calls(t)) != 1 {
		t.Fatal("unsigned request reached inference")
	}
	f.Set(t, testclaude.Config{})
	r.Model.ID = "claude-configured-b"
	_, e = c.Respond(t.Context(), r, llm.RequestOptions{})
	requireCode(t, e, "invalid_effort")
	r.Model.ReasoningEffort = "high"
	if _, e = c.Respond(t.Context(), r, llm.RequestOptions{}); e != nil {
		t.Fatal(e)
	}
	calls := f.Calls(t)
	argument(t, calls[1].Arguments, "--model", "claude-configured-b")
	argument(t, calls[1].Arguments, "--effort", "high")
	r.Tools = []llm.Tool{{Name: "Read"}}
	_, e = c.Respond(t.Context(), r, llm.RequestOptions{})
	requireCode(t, e, "tools_unsupported")
	r.Tools = nil
	r.Input = append(r.Input, llm.Item{Type: llm.ItemToolResult, Data: llm.ToolResult{CallID: "t", Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "result"}}}})
	_, e = c.Respond(t.Context(), r, llm.RequestOptions{})
	requireCode(t, e, "unsupported_input")
}

func TestSubprocessErrorsAndContextReplay(t *testing.T) {
	c, f := fakeClient(t)
	f.Set(t, testclaude.Config{Stream: "secret-token malformed\n"})
	_, e := c.Respond(t.Context(), textRequest(), llm.RequestOptions{})
	requireCode(t, e, "malformed_stream")
	f.Set(t, testclaude.Config{Exit: 17})
	_, e = c.Respond(t.Context(), textRequest(), llm.RequestOptions{})
	requireCode(t, e, "subprocess_failure")
	if strings.Contains(e.Error(), "sensitive") {
		t.Fatal("raw stderr persisted")
	}
	f.Set(t, testclaude.Config{})
	req := textRequest()
	req.Input = append(req.Input, llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "prior answer"}}, llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "second prompt"}})
	if _, e = c.Respond(t.Context(), req, llm.RequestOptions{}); e != nil {
		t.Fatal(e)
	}
	calls := f.Calls(t)
	last := calls[len(calls)-1]
	var messages []llm.Message
	if json.Unmarshal([]byte(last.Input), &messages) != nil || len(messages) != 3 || messages[1].Role != llm.RoleAssistant || messages[1].Text != "prior answer" || messages[2].Text != "second prompt" {
		t.Fatal("canonical text context was not replayed")
	}
}

func TestCancellationReapsProcessGroupAndClosesPipes(t *testing.T) {
	for _, mode := range []ManagedPolicyMode{ManagedPolicyReject, ManagedPolicyTrust} {
		t.Run(string(mode), func(t *testing.T) { testCancellationReapsProcessGroup(t, mode) })
	}
}

func testCancellationReapsProcessGroup(t *testing.T, mode ManagedPolicyMode) {
	t.Helper()
	for _, tc := range []struct {
		name   string
		config testclaude.Config
	}{{"cooperative descendant", testclaude.Config{Wait: true, Child: true}}, {"stubborn executable", testclaude.Config{Wait: true, IgnoreTermination: true}}} {
		t.Run(tc.name, func(t *testing.T) {
			c, f := fakeClient(t)
			c.config.ManagedPolicyMode = mode
			if mode == ManagedPolicyTrust {
				tc.config.Subscription = "team"
			}
			f.Set(t, tc.config)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, e := c.Respond(ctx, textRequest(), llm.RequestOptions{}); done <- e }()
			deadline := time.Now().Add(5 * time.Second)
			var call testclaude.Call
			for {
				calls := f.Calls(t)
				if len(calls) > 0 {
					call = calls[0]
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("fake subprocess did not start")
				}
				time.Sleep(time.Millisecond)
			}
			cancel()
			select {
			case e := <-done:
				if !errors.Is(e, context.Canceled) {
					t.Fatal("cancellation normalized as provider failure", e)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("process/pipes did not drain")
			}
			for _, pid := range []int{call.PID, call.ChildPID} {
				if pid != 0 {
					if e := syscall.Kill(pid, 0); !errors.Is(e, syscall.ESRCH) {
						t.Fatalf("process %d remains alive or zombie: %v", pid, e)
					}
				}
			}
			if _, e := os.Stat(call.Directory); !errors.Is(e, os.ErrNotExist) {
				t.Fatal("request directory not cleaned")
			}
		})
	}
}

func TestClaudeCredentialContentsAreNeverOpenedByAdapter(t *testing.T) {
	for _, mode := range []ManagedPolicyMode{ManagedPolicyReject, ManagedPolicyTrust} {
		t.Run(string(mode), func(t *testing.T) { testCredentialContentsAreNeverOpened(t, mode) })
	}
}

func testCredentialContentsAreNeverOpened(t *testing.T, mode ManagedPolicyMode) {
	t.Helper()
	c, f := fakeClient(t)
	c.config.ManagedPolicyMode = mode
	if mode == ManagedPolicyTrust {
		f.Set(t, testclaude.Config{Subscription: "enterprise"})
	}
	dir := filepath.Join(f.Home, ".claude")
	if e := os.Mkdir(dir, 0700); e != nil {
		t.Fatal(e)
	}
	// Only the executable is allowed to read this source. A direct Go credential
	// parser would block on this FIFO; our native fake owns auth status instead.
	if e := syscall.Mkfifo(filepath.Join(dir, ".credentials.json"), 0600); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, e := c.Respond(ctx, textRequest(), llm.RequestOptions{}); e != nil {
		t.Fatal("adapter attempted to read a Claude credential source", e)
	}
}

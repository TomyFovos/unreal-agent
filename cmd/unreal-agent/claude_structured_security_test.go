//go:build linux || darwin

package main

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudecode"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

func TestClaudeStructuredPermissionWorkspaceAndCommandBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, tool, args, receipt string
		operations                int
	}{
		{"workspace", "read", `{"path":"../outside.txt"}`, "read_failed", 1},
		{"ssh", "read", `{"path":"~/.ssh/id_rsa"}`, "read_failed", 1},
		{"process-deny", "Bash", `{"command":"curl https://network-forbidden.invalid","max_output_length":1024}`, "permission denied", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := testclaude.New(t)
			cfg := bridgeServeConfiguration(t, f)
			cfg.Runtime.ClaudeCode.ToolBridge.Mode = claudecode.BridgeModeStructured
			cfg.Launcher = &normalLauncherConfig{Version: 1, DiscoverProviders: new(false)}
			cfg.Permissions = permission.Config{Tools: []string{"read", "Bash"}, ReadRoots: []string{cfg.Runtime.Workspace}}
			f.Set(t, testclaude.Config{Subscription: "team", BridgeManagedPermissionsOnly: true, BridgeSteps: []testclaude.BridgeStep{{Name: tc.tool, Arguments: tc.args, Error: true, Contains: tc.receipt}}})
			c, _, _ := startInteractiveHost(t, cfg)
			v, err := c.Open(t.Context(), host.Create, "structured-boundary")
			if err != nil {
				t.Fatal(err)
			}
			submitInteractive(t, c, v, "task", "Exercise only the fake configured boundary.")
			end := inspectInteractive(t, c, "structured-boundary", func(v host.View) bool {
				if v.Failure != "" {
					t.Fatal(v.Failure)
				}
				return hasBridgeFinal(v)
			})
			if len(end.Operations) != tc.operations || len(f.Calls(t)) != 2 {
				t.Fatal("boundary bypassed or receipt not returned", len(end.Operations), len(f.Calls(t)))
			}
			for _, call := range f.Calls(t) {
				cliArgument(t, call, "--tools", "")
				cliArgument(t, call, "--allowedTools", "StructuredOutput")
				cliArgument(t, call, "--mcp-config", `{"mcpServers":{}}`)
			}
			if _, err := os.Stat(filepath.Join(cfg.Runtime.Workspace, "forbidden.txt")); !os.IsNotExist(err) {
				t.Fatal("permission denial changed filesystem")
			}
			if report := multiReport(t, end); report.ToolBridgeMode != "structured" || report.ToolCapability != "Unreal tools via structured actions" || report.Tools.Failed != 1 {
				t.Fatal("analysis lost safe bridge/failure metadata", report.ToolBridgeMode, report.ToolCapability, report.Tools.Failed)
			}
		})
	}
}

func TestClaudeStructuredMalformedOutputCannotCreateOperationOrPublicProtocol(t *testing.T) {
	for _, tc := range []struct{ name, raw, event, stage string }{
		{"ambiguous", `{"type":"action","action":{"id":"a","tool":"Bash","arguments":{"command":123}},"final":{"message":"invented success"}}`, "", "structured_action_envelope_invalid"},
		{"arguments", `{"type":"action","action":{"id":"a","tool":"Bash","arguments":{"command":123}}}`, "", "structured_arguments_schema_rejected"},
		{"schema", `{"type":"action","action":{"id":"bad/id","tool":"read","arguments":{"path":"private-sensitive"}}}`, "", "structured_schema_rejected"},
		{"unknown-tool", `{"type":"action","action":{"id":"a","tool":"unknown","arguments":{}}}`, "", "structured_unknown_tool"},
		{"missing", "", `{"type":"result","subtype":"success","is_error":false,"result":"private-sensitive"}`, "structured_result_missing_output"},
		{"result-execution", "", `{"type":"result","subtype":"success","is_error":false,"structured_output":{"type":"action","action":{"id":"a","tool":"read","arguments":{"path":"private-sensitive"}}},"tool_use_result":{"output":"private-sensitive"}}`, "structured_execution_rejected"},
		{"trailing-execution", "", `{"type":"result","subtype":"success","is_error":false,"structured_output":{"type":"action","action":{"id":"a","tool":"read","arguments":{"path":"private-sensitive"}}}}` + "\n" + `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"private-sensitive"}}]}}`, "structured_trailing_frame"},
		{"malformed-assistant-before-result", "", `{"type":"assistant","message":{"model":"<synthetic>","content":[{"type":"text","text":{}}]}}`, "structured_serializer_frame_invalid"},
		{"builtin-before-result", "", `{"type":"assistant","message":{"model":"<synthetic>","content":[{"type":"tool_use","id":"helper","name":"StructuredOutput","input":{}},{"type":"tool_use","id":"private-helper-id","name":"Bash","input":{"command":"private-sensitive"}}]}}`, "structured_execution_rejected"},
		{"wire-injected-execution", "", `{"type":"assistant","message":{"model":"<synthetic>","content":[{"type":"tool_use","id":"helper","name":"StructuredOutput","input":{}}]},"wire_tool_inputs":{"helper":{"nested":{"type":"tool_use","name":"Bash","input":{"command":"private-sensitive"}}}}}`, "structured_serializer_frame_invalid"},
		{"wire-unrelated-id", "", `{"type":"assistant","message":{"model":"<synthetic>","content":[{"type":"tool_use","id":"helper","name":"StructuredOutput","input":{}}]},"wire_tool_inputs":{"private-helper-id":{}}}`, "structured_serializer_frame_invalid"},
		{"wire-unresolved-message", "", `{"type":"assistant","message":{"model":"<synthetic>","content":[{"type":"tool_use","id":"helper","name":"StructuredOutput","input":{}}],"future_metadata_field":["private-sensitive",{"session_id":"private-value-session-id","input":"private-value-input"}]},"wire_tool_inputs":{"helper":{}}}`, "structured_serializer_frame_invalid"},
		{"input-transformations-null", "", `{"type":"assistant","message":{"model":"<synthetic>","content":[{"type":"tool_use","id":"helper","name":"StructuredOutput","input":{}}],"input_transformations":null},"wire_tool_inputs":{"helper":{}}}`, "structured_serializer_frame_invalid"},
		{"input-transformations-execution", "", `{"type":"assistant","message":{"model":"<synthetic>","content":[{"type":"tool_use","id":"helper","name":"StructuredOutput","input":{}}],"input_transformations":[{"type":"tool_use","name":"Bash","input":{"command":"private-sensitive"}}]},"wire_tool_inputs":{"helper":{}}}`, "structured_serializer_frame_invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := testclaude.New(t)
			cfg := bridgeServeConfiguration(t, f)
			cfg.Runtime.ClaudeCode.ToolBridge.Mode = claudecode.BridgeModeStructured
			fake := testclaude.Config{Subscription: "team", StructuredResponses: []string{tc.raw}}
			if tc.event != "" {
				fake.Stream = `{"type":"system","subtype":"init","model":"synthetic-model","tools":["StructuredOutput"],"mcp_servers":[],"permissionMode":"default"}` + "\n" + tc.event + "\n"
			}
			f.Set(t, fake)
			c, _, _ := startInteractiveHost(t, cfg)
			v, err := c.Open(t.Context(), host.Create, "structured-malformed")
			if err != nil {
				t.Fatal(err)
			}
			submitInteractive(t, c, v, "task", "Run the offline malformed fixture.")
			end := inspectInteractive(t, c, "structured-malformed", func(v host.View) bool { return v.Failure != "" })
			if !strings.Contains(end.Failure, "stage="+tc.stage) || strings.Contains(end.Failure, "private-sensitive") || len(end.Operations) != 0 || responseCount(end) != 0 || len(f.Calls(t)) != 1 {
				t.Fatal("invalid protocol became work/public conversation or lost safe stage", end.Failure, len(end.Operations), responseCount(end))
			}
			if tc.name == "wire-unresolved-message" && !strings.Contains(end.Failure, "metadata_scope=message metadata_field=future_metadata_field ") {
				t.Fatal("safe rejected message name did not reach Host failure")
			}
			encoded, err := json.Marshal(end.History)
			if err != nil || strings.Contains(string(encoded), "private-sensitive") || strings.Contains(string(encoded), "private-helper-id") || strings.Contains(string(encoded), "private-value-session-id") || strings.Contains(string(encoded), "private-value-input") || strings.Contains(end.Failure, "private-value-session-id") || strings.Contains(end.Failure, "private-value-input") {
				t.Fatal("private rejected protocol metadata entered canonical history", err)
			}
		})
	}
}

func TestClaudeStructuredSDKAssistantCreatesOnlyAuthoritativeCanonicalOperation(t *testing.T) {
	f := testclaude.New(t)
	cfg := bridgeServeConfiguration(t, f)
	cfg.Runtime.ClaudeCode.ToolBridge.Mode = claudecode.BridgeModeStructured
	const content = "canonical read receipt; provider did not read this file"
	if err := os.WriteFile(filepath.Join(cfg.Runtime.Workspace, "fixture.txt"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	// Installed SDK shape: optional message.id, no assistant subtype, nullable
	// stop metadata, private envelope ids, direct serializer caller/display label.
	const frame = `{"type":"assistant","message":{"role":"assistant","type":"message","model":"<synthetic>","content":[{"type":"tool_use","id":"serializer-only","name":"StructuredOutput","input":{"internal":"private-sensitive"},"caller":{"type":"direct"}}],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0},"container":null,"input_transformations":[{"type":"thinking_dropped","path":"private-transform-sensitive","reason":"private-transform-sensitive"}]},"parent_tool_use_id":null,"uuid":"private-helper-id","session_id":"private-session-id","timestamp":"2000-01-01T00:00:00Z","tool_use_meta":[{"id":"serializer-only","display_name":"private-sensitive"}],"wire_tool_inputs":{"serializer-only":{"private_transport":"private-wire-sensitive"}}}`
	f.Set(t, testclaude.Config{Subscription: "team", BridgeManagedPermissionsOnly: true,
		BridgeSteps:               []testclaude.BridgeStep{{Name: "read", Arguments: `{"path":"fixture.txt"}`, Contains: content, Duplicate: true}},
		StructuredAssistantFrames: []string{frame, frame, frame},
	})
	c, _, _ := startInteractiveHost(t, cfg)
	v, err := c.Open(t.Context(), host.Create, "structured-sdk-assistant")
	if err != nil {
		t.Fatal(err)
	}
	submitInteractive(t, c, v, "task", "Run only the fake read action.")
	end := inspectInteractive(t, c, "structured-sdk-assistant", func(v host.View) bool {
		if v.Failure != "" {
			t.Fatal(v.Failure)
		}
		return hasBridgeFinal(v)
	})
	// One canonical tool-call response and one final response; replay reuses the
	// existing receipt without appending another model tool-call or Operation.
	if len(end.Operations) != 1 || responseCount(end) != 2 || len(f.Calls(t)) != 3 {
		t.Fatal("helper/replay created extra work or final missing", len(end.Operations), responseCount(end), len(f.Calls(t)))
	}
	for _, call := range f.Calls(t) {
		cliArgument(t, call, "--tools", "")
		cliArgument(t, call, "--allowedTools", "StructuredOutput")
		cliArgument(t, call, "--mcp-config", `{"mcpServers":{}}`)
	}
	// The helper's internal object was never the execution proposal. The fake
	// checks the actual canonical read receipt in each subsequent generation.
	for _, op := range end.Operations {
		if op.ToolName != "read" || op.Status != operation.StatusCompleted {
			t.Fatal("serializer became an Operation or receipt not completed", op.ToolName, op.Status)
		}
	}
	publicMessages := 0
	for _, item := range end.History.Items {
		if r, ok := item.Data.(sessionstore.ModelResponse); ok {
			for _, output := range r.Response.Output {
				if message, ok := output.Data.(llm.Message); ok {
					publicMessages++
					if message.Text != "Bridge fixture completed" {
						t.Fatal("helper became public prose")
					}
				}
			}
		}
	}
	encoded, err := json.Marshal(end.History)
	if err != nil || publicMessages != 1 {
		t.Fatal("canonical final missing", err, publicMessages)
	}
	for _, private := range []string{"private-sensitive", "private-wire-sensitive", "wire_tool_inputs", "private-transform-sensitive", "input_transformations", "private-helper-id", "private-session-id", "<synthetic>", "StructuredOutput"} {
		if strings.Contains(string(encoded), private) {
			t.Fatal("private assistant helper entered canonical history")
		}
	}
}

func TestClaudeStructuredSDKProviderErrorsCreateNoOperationOrPublicResponse(t *testing.T) {
	// All locally pinned 0.3.285 enum values, plus a non-SDK value. The error
	// precedes any proposal: no callback/execution/public completion is possible.
	for _, value := range []string{"authentication_failed", "oauth_org_not_allowed", "account_on_hold", "verification_required", "billing_error", "rate_limit", "overloaded", "invalid_request", "model_not_found", "server_error", "unknown", "max_output_tokens", "cloud_credential_error", "private-sensitive authentication_failed"} {
		reason := "assistant_" + value
		if strings.HasPrefix(value, "private-sensitive") {
			reason = "assistant_provider_error_unknown"
		}
		t.Run(reason, func(t *testing.T) {
			f := testclaude.New(t)
			cfg := bridgeServeConfiguration(t, f)
			cfg.Runtime.ClaudeCode.ToolBridge.Mode = claudecode.BridgeModeStructured
			event, err := json.Marshal(map[string]any{
				"type": "assistant", "error": value, "uuid": "private-helper-id", "session_id": "private-session-id",
				"message": map[string]any{"model": "<synthetic>", "content": []any{map[string]any{"type": "text", "text": "private-sensitive raw-provider-body"}}},
			})
			if err != nil {
				t.Fatal(err)
			}
			f.Set(t, testclaude.Config{Subscription: "team", Stream: `{"type":"system","subtype":"init","model":"synthetic-model","tools":["StructuredOutput"],"mcp_servers":[],"permissionMode":"default"}` + "\n" + string(event) + "\n"})
			c, _, _ := startInteractiveHost(t, cfg)
			v, err := c.Open(t.Context(), host.Create, "structured-provider-error")
			if err != nil {
				t.Fatal(err)
			}
			submitInteractive(t, c, v, "task", "Run the offline provider-error fixture.")
			end := inspectInteractive(t, c, "structured-provider-error", func(v host.View) bool { return v.Failure != "" })
			if !strings.Contains(end.Failure, "stage=structured_result_invalid") || !strings.Contains(end.Failure, "reason="+reason) || len(end.Operations) != 0 || responseCount(end) != 0 || len(f.Calls(t)) != 1 {
				t.Fatal("provider failure lost enum identity or became execution/retry/completion", end.Failure, len(end.Operations), responseCount(end))
			}
			encoded, err := json.Marshal(end.History)
			for _, private := range []string{"private-sensitive", "private-helper-id", "private-session-id", "raw-provider-body"} {
				if err != nil || strings.Contains(end.Failure, private) || strings.Contains(string(encoded), private) {
					t.Fatal("provider error body or identity leaked", err)
				}
			}
			call := f.Calls(t)[0]
			cliArgument(t, call, "--tools", "")
			cliArgument(t, call, "--allowedTools", "StructuredOutput")
			cliArgument(t, call, "--mcp-config", `{"mcpServers":{}}`)
		})
	}
}

//go:build linux || darwin

package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/host/gateway"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudecode"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/profile"
	"github.com/unreallabsai/unreal-agent/harness/provider"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

var realStructuredRounds = flag.Bool("claude-structured-real-round-probe", false, "explicit real Claude opt-in: read one temporary fixture through an Unreal Operation, then Final")
var realStructuredBinary = flag.String("claude-structured-real-binary", "", "absolute Claude CLI for the opt-in round probe")
var realStructuredModel = flag.String("claude-structured-real-model", "", "explicit Claude model alias for the opt-in round probe")

// No normal configuration, existing state/socket/Session or AIdea path is used.
// The only executor permission is read within an isolated temporary workspace.
// A marker known only to the fixture file proves that the canonical receipt,
// rather than a prompt echo, reached the subsequent real generation.
func TestClaudeStructuredRealAdapterRoundProbe(t *testing.T) {
	if !*realStructuredRounds {
		t.Skip("real Claude inference disabled; explicit round-probe opt-in required")
	}
	if !filepath.IsAbs(*realStructuredBinary) || *realStructuredModel == "" {
		t.Fatal("explicit real adapter configuration required")
	}
	testStructuredAdapterRoundProbe(t, *realStructuredBinary, *realStructuredModel)
}

func structuredRoundProbeConfiguration(workspace, binary, model string) serveConfiguration {
	return serveConfiguration{
		Launcher: &normalLauncherConfig{Version: 1, DiscoverProviders: new(false)},
		Context:  contextengine.Config{Version: 1, InputBudget: 12000, RecentReserve: 6000, RetrievalLimit: 4},
		Runtime: agentrunner.RuntimeIdentity{
			Version: 1, Workspace: workspace, SystemPrompt: "Use only Unreal-owned read. Observe the canonical receipt before reporting the file content. No other work is permitted.", ReasoningEffort: llm.ReasoningEffortHigh, Profile: profile.Default(),
			Provider:   provider.Selection{Version: 1, Provider: "claude-code", Model: provider.Model{ID: model, Family: "claude", Capabilities: []string{"reasoning"}}, Auth: credential.Reference{Provider: "claude-code", Method: credential.OAuth, ID: "external-claude-code"}, MaxAttempts: 1, Source: "explicit adapter probe"},
			ClaudeCode: &claudecode.Config{Binary: binary, ManagedPolicyMode: claudecode.ManagedPolicyTrust, ToolBridge: claudecode.ToolBridgeConfig{Enabled: true, Mode: claudecode.BridgeModeStructured, MaxActionRounds: 2, TimeoutMillis: 120000, ResultBytes: 4096}, Models: []modelcatalog.Model{{ID: model, Name: "Explicit probe model", Efforts: []llm.ReasoningEffort{llm.ReasoningEffortHigh}, DefaultEffort: llm.ReasoningEffortHigh}}},
		},
		Permissions: permission.Config{Tools: []string{"read"}, ReadRoots: []string{workspace}},
	}
}

func TestClaudeStructuredRoundProbeConfiguration(t *testing.T) {
	cfg := structuredRoundProbeConfiguration(t.TempDir(), "/unused/fake-claude", "fixture-opus")
	registry, err := registeredProviders(cfg.Runtime, cfg.Providers)
	if err != nil {
		t.Fatal("round probe provider registration invalid")
	}
	if _, err := registry.Resolve(cfg.Runtime.Provider); err != nil {
		t.Fatal("round probe RuntimeSelection invalid")
	}
	// Reproduce the pre-generation harness failure without launching a CLI.
	cfg.Runtime.Provider.Model.Capabilities = append(cfg.Runtime.Provider.Model.Capabilities, "tools")
	_, err = registeredProviders(cfg.Runtime, cfg.Providers)
	var providerError *provider.Error
	if !errors.As(err, &providerError) || providerError.Code != "invalid_model_capability" {
		t.Fatal("static model capability and runtime bridge capability were conflated")
	}
	policy, err := permission.New(cfg.Permissions)
	if err != nil {
		t.Fatal("probe read policy invalid")
	}
	defer policy.Close()
	if policy.CheckTool("read") != nil || policy.CheckTool("Bash") == nil || policy.CheckProcess() == nil {
		t.Fatal("round probe expanded executor permission")
	}
}

const structuredRoundProbeMarker = "UNREAL_REAL_ADAPTER_RECEIPT_7F2B"

func TestClaudeStructuredRoundProbeFakeHost(t *testing.T) {
	f := testclaude.New(t)
	t.Setenv("HOME", f.Home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	f.Set(t, testclaude.Config{Subscription: "team", BridgeManagedPermissionsOnly: true, StructuredResponses: []string{
		`{"type":"action","action":{"id":"probe_read","tool":"read","arguments":{"path":"probe.txt"}}}`,
		`{"type":"final","final":{"message":"` + structuredRoundProbeMarker + `"}}`,
	}})
	testStructuredAdapterRoundProbe(t, f.Binary, "fixture-opus")
	if len(f.Calls(t)) != 2 {
		t.Fatal("read/Final harness duplicated a generation")
	}
}

func testStructuredAdapterRoundProbe(t *testing.T, binary, model string) {
	t.Helper()
	workspace := t.TempDir()
	const marker = structuredRoundProbeMarker
	if err := os.WriteFile(filepath.Join(workspace, "probe.txt"), []byte(marker+"\n"), 0600); err != nil {
		t.Fatal("fixture setup failed")
	}
	cfg := structuredRoundProbeConfiguration(workspace, binary, model)
	directory := privateCLIDirectory(t)
	configData, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal("probe configuration encoding failed")
	}
	configPath, socket := filepath.Join(directory, "config.json"), filepath.Join(directory, "host.sock")
	if os.WriteFile(configPath, configData, 0600) != nil {
		t.Fatal("private config creation failed")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	done := make(chan error, 1)
	output := &credentialOutput{}
	go func() {
		done <- run(ctx, []string{"serve", "--config", configPath, "--session-directory", filepath.Join(directory, "sessions"), "--socket", socket}, output)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error("probe Host shutdown failed")
			}
		case <-time.After(10 * time.Second):
			t.Error("probe Host did not drain")
		}
	})
	client := gateway.NewClient(socket)
	t.Cleanup(func() { _ = client.Close() })
	ready := time.NewTicker(25 * time.Millisecond)
	defer ready.Stop()
	for {
		if _, err := client.Methods(ctx); err == nil {
			break
		}
		select {
		case err := <-done:
			done <- err
			t.Fatal("probe Host startup failed")
		case <-ctx.Done():
			t.Fatal("probe Host readiness timeout")
		case <-ready.C:
		}
	}
	v, err := client.Open(ctx, host.Create, "real-adapter-round")
	if err != nil {
		t.Fatal("probe Session creation failed")
	}
	submitInteractive(t, client, v, "read-probe", "Read probe.txt exactly once using Unreal's read action. After receiving its machine-owned result, return a Final whose message is exactly the trimmed file content, with no prefix or suffix. Do not guess the content. No other actions are permitted.")
	var end host.View
	completed := false
	for !completed {
		end, err = client.Inspect(ctx, "real-adapter-round", 0, 256)
		if err != nil {
			t.Fatal("probe projection failed")
		}
		if end.Failure != "" {
			t.Fatal("real adapter round failed", end.Failure)
		}
		for _, item := range end.History.Items {
			if response, ok := item.Data.(sessionstore.ModelResponse); ok {
				for _, output := range response.Response.Output {
					if message, ok := output.Data.(llm.Message); ok {
						if message.Text != marker {
							t.Fatal("Final did not match the machine-owned receipt")
						}
						completed = true
					}
				}
			}
		}
		if !completed {
			select {
			case <-ctx.Done():
				t.Fatal("real adapter round timed out")
			case <-ready.C:
			}
		}
	}
	if len(end.Operations) != 1 || responseCount(end) != 2 {
		t.Fatal("Action/Final loop created extra work or lost canonical responses")
	}
	for _, op := range end.Operations {
		if op.ToolName != "read" || op.Status != operation.StatusCompleted || op.Denial != nil {
			t.Fatal("read did not complete through canonical permission/Operation")
		}
	}
	encoded, _ := json.Marshal(end.History.Items)
	for _, private := range []string{"StructuredOutput", `\"type\":\"action\"`, "wire_tool_inputs", "input_transformations", "request_id", "session_id"} {
		if strings.Contains(string(encoded), private) {
			t.Fatal("private protocol entered canonical history")
		}
	}
	if end.Context == nil || end.Context.EstimatedInputTokens > end.Context.Budget.Input {
		t.Fatal("real bridge bypassed bounded context")
	}
	content, err := os.ReadFile(filepath.Join(workspace, "probe.txt"))
	if err != nil || string(content) != marker+"\n" {
		t.Fatal("read-only probe changed its fixture")
	}
	t.Log("adapter Action(read) -> canonical Operation/receipt -> next generation -> durable Final PASS; Operations=1; MCP=0; Claude side-effecting built-ins=0; workspace changes=0")
}

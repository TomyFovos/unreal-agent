//go:build linux || darwin

package main

import (
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/cmd/internal/tui"
	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudecode"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

func bridgeServeConfiguration(t *testing.T, f *testclaude.Fixture) serveConfiguration {
	t.Helper()
	cfg := claudeServeConfiguration(t, f)
	cfg.Runtime.SystemPrompt = "Use the Unreal tools for this workspace. Report the observed results."
	cfg.Runtime.ClaudeCode.ManagedPolicyMode = claudecode.ManagedPolicyTrust
	cfg.Runtime.ClaudeCode.ToolBridge.Enabled = true
	cfg.Permissions.Tools = []string{"read", "grep", "glob", "write", "edit", "Bash"}
	return cfg
}

func hasBridgeFinal(v host.View) bool {
	for _, i := range v.History.Items {
		if r, ok := i.Data.(sessionstore.ModelResponse); ok {
			for _, output := range r.Response.Output {
				if m, ok := output.Data.(llm.Message); ok && m.Text == "Bridge fixture completed" {
					return true
				}
			}
		}
	}
	return false
}

func TestClaudeBridgeHostFailFixPassCanonicalOperationsAndRestart(t *testing.T) {
	testClaudeBridgeHost(t, claudecode.BridgeModeMCP)
}

func TestClaudeStructuredHostFailFixPassCanonicalOperationsAndRestart(t *testing.T) {
	testClaudeBridgeHost(t, claudecode.BridgeModeStructured)
}

func testClaudeBridgeHost(t *testing.T, mode string) {
	t.Helper()
	// Resolve the compiler cache before the fake provider replaces HOME. These
	// shell receipts test project compilation, not a fresh standard-library
	// bootstrap inside every isolated auth home. Keep the existing 5s deadline.
	cache, err := exec.Command("go", "env", "GOCACHE").Output()
	if err != nil {
		t.Fatal("resolve fixture Go build cache", err)
	}
	t.Setenv("GOCACHE", strings.TrimSpace(string(cache)))
	f := testclaude.New(t)
	cfg := bridgeServeConfiguration(t, f)
	cfg.Runtime.ClaudeCode.ToolBridge.Mode = mode
	cfg.Context = contextengine.Config{Version: 1, InputBudget: 24000, RecentReserve: 7000, RetrievalLimit: 8}
	workspace := cfg.Runtime.Workspace
	for name, text := range map[string]string{
		"go.mod":         "module bridgefixture\n\ngo 1.24\n",
		"answer.go":      "package bridgefixture\n\nfunc Answer() int { return 0 }\n",
		"answer_test.go": "package bridgefixture\n\nimport \"testing\"\n\nfunc TestAnswer(t *testing.T) { if Answer() != 42 { t.Fatal(\"answer is incorrect\") } }\n",
		"AGENTS.md":      "PROJECT_INSTRUCTIONS_BRIDGE_SNAPSHOT",
	} {
		if err := os.WriteFile(filepath.Join(workspace, name), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// A temporary index supplies a diff without making a commit.
	for _, args := range [][]string{{"init", "--quiet"}, {"add", "answer.go", "answer_test.go", "go.mod"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = workspace
		if err := cmd.Run(); err != nil {
			t.Fatal(err)
		}
	}
	steps := []testclaude.BridgeStep{
		{Name: "read", Arguments: `{"path":"answer.go"}`, Contains: "return 0"},
		{Name: "grep", Arguments: `{"path":".","pattern":"Answer","glob":"*.go"}`, Contains: "answer_test.go"},
		{Name: "edit", Arguments: `{"path":"answer.go","expected":{"exists":true,"digest":"$REVISION"},"old_text":"return 0","new_text":"return 1"}`, Contains: "applied", Duplicate: true},
		{Name: "Bash", Arguments: `{"command":"go test ./...","max_output_length":2048}`, Error: true, Contains: "answer is incorrect"},
		{Name: "read", Arguments: `{"path":"answer.go"}`, Contains: "return 1"},
		{Name: "edit", Arguments: `{"path":"answer.go","expected":{"exists":true,"digest":"$REVISION"},"old_text":"return 1","new_text":"return 42"}`, Contains: "applied"},
		{Name: "write", Arguments: `{"path":"created.md","expected":{"exists":false},"content":"# Created via Unreal\n日本語 😀\n"}`, Contains: "applied"},
		{Name: "glob", Arguments: `{"path":".","glob":"*.md"}`, Contains: "created.md"},
		{Name: "Bash", Arguments: `{"command":"go test ./...","max_output_length":2048}`, Contains: "ok"},
		{Name: "Bash", Arguments: `{"command":"git status --short","max_output_length":2048}`, Contains: "answer.go"},
		{Name: "Bash", Arguments: `{"command":"git diff -- answer.go","max_output_length":2048}`, Contains: "return 42"},
	}
	if mode == claudecode.BridgeModeStructured {
		steps = append([]testclaude.BridgeStep{{Name: "glob", Arguments: `{"path":".","glob":"*.go"}`, Contains: "answer.go"}, {Name: "read", Arguments: `{"path":"AGENTS.md"}`, Contains: "DISK_CHANGED_AFTER_SESSION_CREATION"}}, steps...)
	}
	fake := testclaude.Config{Subscription: "team", BridgeSteps: steps}
	if mode == claudecode.BridgeModeStructured {
		// The SDK's public result is authoritative even when serializer input
		// is an internal representation. Exercise the actual Host/Operation/
		// receipt/context loop, including duplicate replay and fail->fix->pass.
		fake.StructuredHelperInputs = make([]string, len(steps)+2)
		for i := range fake.StructuredHelperInputs {
			fake.StructuredHelperInputs[i] = `{"serializer_internal":{"message":"private-helper-sensitive"}}`
		}
	}
	f.Set(t, fake)
	directory := privateCLIDirectory(t)
	c, shutdown, output := startClaudeStreamHost(t, cfg, directory)
	v, err := c.Open(t.Context(), host.Create, "bridge-test")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(workspace, "AGENTS.md"), []byte("DISK_CHANGED_AFTER_SESSION_CREATION"), 0600); err != nil {
		t.Fatal(err)
	}
	submitInteractive(t, c, v, "bridge-input", "Inspect, fix, test, and report this fixture.")
	done := inspectInteractive(t, c, "bridge-test", func(view host.View) bool {
		if view.Failure != "" {
			t.Fatal("bridge turn failed", view.Failure)
		}
		return hasBridgeFinal(view)
	})
	wantProcesses := 1
	if mode == claudecode.BridgeModeStructured {
		wantProcesses = len(steps) + 2
	}
	if len(f.Calls(t)) != wantProcesses || responseCount(done) != len(steps)+1 || len(done.Operations) != len(steps) {
		t.Fatal("bridge duplicated execution or lost canonical responses", len(f.Calls(t)), responseCount(done), len(done.Operations))
	}
	if done.Context == nil || done.Context.EstimatedInputTokens > done.Context.Budget.Input {
		t.Fatal("bridge bypassed the shared bounded Context Engine")
	}
	budget := done.Context.Budget
	if budget.Input != min(int64(24000), budget.Window-budget.ResponseReserve-budget.SchemaReserve-budget.ProtocolReserve) || budget.SchemaReserve == 0 {
		t.Fatal("bridge did not reserve the selected tool schemas")
	}
	call := f.Calls(t)[0]
	if !strings.Contains(call.System, "PROJECT_INSTRUCTIONS_BRIDGE_SNAPSHOT") || strings.Contains(call.System, "DISK_CHANGED_AFTER_SESSION_CREATION") {
		t.Fatal("Project Instructions snapshot replaced")
	}
	cliArgument(t, call, "--tools", "")
	cliArgument(t, call, "--strict-mcp-config", "--mcp-config")
	if !slices.Contains(call.Arguments, "--safe-mode") || slices.Contains(call.Arguments, "--bare") {
		t.Fatal("Claude execution isolation changed")
	}
	var order []string
	results, continuation, failures := 0, 0, 0
	for _, i := range done.History.Items {
		switch data := i.Data.(type) {
		case session.Turn:
			if data.RuntimeRevision != 0 {
				t.Fatal("tool round silently changed runtime revision")
			}
			if data.ToolContinuation != "" {
				continuation++
			}
		case sessionstore.ModelResponse:
			for _, item := range data.Response.Output {
				if item.Type == llm.ItemReasoning || item.ProviderID != "" {
					t.Fatal("private Claude continuation persisted")
				}
				if tool, ok := item.Data.(llm.ToolCall); ok {
					order = append(order, tool.Name)
				}
			}
		case sessionstore.ToolCallStatus:
			if len(data.Operations) != 1 || data.Operations[0].Status != operation.StatusCompleted {
				continue
			}
			results++
		}
	}
	for _, op := range done.Operations {
		if op.Status != operation.StatusCompleted {
			t.Fatal("Operation did not reach terminal completion", op.ToolName, op.Status)
		}
		if op.Type == operation.TypeShell {
			state, e := operation.DecodeShellState(op)
			if e != nil || state.Result == nil {
				t.Fatal("shell receipt not canonical", e)
			}
			if state.Result.ExitCode != 0 {
				failures++
			}
		}
	}
	want := []string{}
	for _, step := range steps {
		want = append(want, step.Name)
	}
	if !slices.Equal(order, want) || continuation != len(steps) || results != len(steps) || failures != 1 {
		t.Fatal("canonical round/receipt ordering", order, continuation, results, failures)
	}
	content, e := os.ReadFile(filepath.Join(workspace, "answer.go"))
	if e != nil || !strings.Contains(string(content), "return 42") {
		t.Fatal("Unreal edits not applied", e)
	}
	content, e = os.ReadFile(filepath.Join(workspace, "created.md"))
	if e != nil || string(content) != "# Created via Unreal\n日本語 😀\n" {
		t.Fatal("Unreal create not applied", e)
	}
	report := multiReport(t, done)
	wantUsage := int64((len(steps) + 1) * 10)
	if mode == claudecode.BridgeModeStructured {
		wantUsage += 10
	}
	if !report.ToolBridgeEnabled || report.Tools.Calls != len(steps) || report.Usage.Input != wantUsage {
		t.Fatal("shared /analyze projection lost bridge receipts or double counted usage", report.Tools.Calls, report.Usage.Input)
	}
	snapshot := tui.Snapshot{ID: "bridge-test", Operations: done.Operations, Connected: true, Running: true}
	topology := tui.BuildOrchestration(snapshot, viewer.PanelSnapshot{}, time.Now(), 128)
	nodes := 0
	for _, node := range topology.Nodes {
		if node.Kind == "operation" {
			nodes++
		}
	}
	if nodes != len(steps) {
		t.Fatal("Orchestration did not use the shared Operation projection", nodes)
	}
	if _, err = c.Stop(t.Context(), "bridge-test", v.Generation, inbox.StopWhenIdle, "test"); err != nil {
		t.Fatal(err)
	}
	inspectInteractive(t, c, "bridge-test", func(v host.View) bool { return !v.Running })
	shutdown()
	f.Set(t, testclaude.Config{Subscription: "team"})
	c2, _, _ := startClaudeStreamHost(t, cfg, directory)
	v2, err := c2.Open(t.Context(), host.Resume, "bridge-test")
	if err != nil {
		t.Fatal("canonical continuation could not resume", err)
	}
	if len(f.Calls(t)) != wantProcesses {
		t.Fatal("resume replayed a completed tool round")
	}
	submitInteractive(t, c2, v2, "after-resume", "Summarize the completed fixture.")
	done2 := inspectInteractive(t, c2, "bridge-test", func(v host.View) bool { return responseCount(v) == len(steps)+2 })
	if len(done2.Operations) != len(steps) {
		t.Fatal("resume recreated Operations")
	}
	assertNoStoredCredentials(t, filepath.Join(directory, "sessions"), "private-reasoning-sensitive", "private-session-sensitive", "private-cli-result-sensitive", "plugin-catalog-sensitive", "token-sensitive", "account-sensitive")
	assertNoCredentialLeak(t, output.String(), "token-sensitive", "private-api-key-sensitive")
}

func TestClaudeBridgeHardStopWhileWaitingForCanonicalOperation(t *testing.T) {
	testClaudeBridgeHardStop(t, claudecode.BridgeModeMCP)
}

func TestClaudeStructuredHardStopWhileWaitingForCanonicalOperation(t *testing.T) {
	testClaudeBridgeHardStop(t, claudecode.BridgeModeStructured)
}

func testClaudeBridgeHardStop(t *testing.T, mode string) {
	t.Helper()
	f := testclaude.New(t)
	cfg := bridgeServeConfiguration(t, f)
	cfg.Runtime.ClaudeCode.ToolBridge.Mode = mode
	f.Set(t, testclaude.Config{Subscription: "team", BridgeSteps: []testclaude.BridgeStep{{Name: "Bash", Arguments: `{"command":"sleep 30","max_output_length":1024}`, Error: true}}})
	c, _, _ := startInteractiveHost(t, cfg)
	v, err := c.Open(t.Context(), host.Create, "bridge-stop")
	if err != nil {
		t.Fatal(err)
	}
	submitInteractive(t, c, v, "wait", "Run the cancelable fixture command.")
	running := inspectInteractive(t, c, "bridge-stop", func(v host.View) bool {
		if len(v.Operations) != 1 {
			return false
		}
		state, e := operation.DecodeShellState(v.Operations[0])
		return e == nil && state.ProcessGroupID > 0
	})
	if _, err := c.Stop(t.Context(), "bridge-stop", v.Generation, inbox.StopHard, "cancel fixture"); err != nil {
		t.Fatal(err)
	}
	stopped := inspectInteractive(t, c, "bridge-stop", func(v host.View) bool { return !v.Running })
	if len(running.Operations) != 1 || len(stopped.Operations) != 1 || stopped.Operations[0].Status != operation.StatusCanceled || hasBridgeFinal(stopped) {
		t.Fatal("cancelled Operation became successful or survived the writer", stopped.Operations)
	}
	if len(f.Calls(t)) != 1 {
		t.Fatal("stop spawned a replacement provider request")
	}
}

func TestClaudeBridgeSelectionAndQueuedInputWaitForFinalBoundary(t *testing.T) {
	testClaudeBridgeSelection(t, claudecode.BridgeModeMCP)
}

func TestClaudeStructuredSelectionAndQueuedInputWaitForFinalBoundary(t *testing.T) {
	testClaudeBridgeSelection(t, claudecode.BridgeModeStructured)
}

func testClaudeBridgeSelection(t *testing.T, mode string) {
	t.Helper()
	f := testclaude.New(t)
	cfg := bridgeServeConfiguration(t, f)
	cfg.Runtime.ClaudeCode.ToolBridge.Mode = mode
	if err := os.WriteFile(filepath.Join(cfg.Runtime.Workspace, "readme.txt"), []byte("read receipt"), 0600); err != nil {
		t.Fatal(err)
	}
	gate := filepath.Join(f.Directory, "release")
	f.Set(t, testclaude.Config{Subscription: "team", Gate: gate, BridgeOnce: true, BridgeSteps: []testclaude.BridgeStep{{Name: "read", Arguments: `{"path":"readme.txt"}`, Contains: "read receipt"}}})
	c, _, _ := startInteractiveHost(t, cfg)
	v, err := c.Open(t.Context(), host.Create, "bridge-boundary")
	if err != nil {
		t.Fatal(err)
	}
	submitInteractive(t, c, v, "first", "Read the fixture.")
	waitInteractive(t, func() bool { return len(f.Calls(t)) == 1 })
	multiSelect(t, c, v, 0, "claude-code", "claude-configured-b", "high")
	submitInteractive(t, c, v, "queued", "Respond to this separately after the tool finishes.")
	if err := os.WriteFile(gate, []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	end := inspectInteractive(t, c, "bridge-boundary", func(v host.View) bool { return responseCount(v) == 3 })
	calls := f.Calls(t)
	wantProcesses := 2
	if mode == claudecode.BridgeModeStructured {
		wantProcesses = 3
	}
	if len(calls) != wantProcesses {
		t.Fatal("queued input lost or bridge recreated an intermediate model request", len(calls))
	}
	cliArgument(t, calls[0], "--model", "claude-configured-a")
	cliArgument(t, calls[0], "--effort", "medium")
	last := calls[len(calls)-1]
	cliArgument(t, last, "--model", "claude-configured-b")
	cliArgument(t, last, "--effort", "high")
	for _, call := range calls[:len(calls)-1] {
		cliArgument(t, call, "--model", "claude-configured-a")
		cliArgument(t, call, "--effort", "medium")
		if strings.Contains(call.Input, "Respond to this separately") {
			t.Fatal("queued input leaked before the logical final boundary")
		}
	}
	if strings.Contains(calls[0].Input, "Respond to this separately") || !strings.Contains(last.Input, "Respond to this separately") {
		t.Fatal("queued input was acknowledged before reaching the provider")
	}
	report := multiReport(t, end)
	if len(end.Operations) != 1 || len(report.Turns) != 3 || report.Turns[0].Selection.Revision != 0 || report.Turns[1].Selection.Revision != 0 || report.Turns[2].Selection.Revision != 1 || !report.ToolBridgeEnabled {
		t.Fatal("runtime boundary or capability changed inside bridge continuation")
	}
}

func TestClaudeBridgeHostPermissionAndSchemaFailureReturnsCanonicalFeedback(t *testing.T) {
	for _, tc := range []struct {
		name, tool, arguments, contains string
		operations                      int
	}{
		{"schema", "read", `{"path":1}`, "required string field missing", 0},
		{"permission", "write", `{"path":"not-authorized.txt","expected":{"exists":false},"content":"must not write"}`, "denied", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := testclaude.New(t)
			cfg := bridgeServeConfiguration(t, f)
			cfg.Permissions.Tools = []string{"read"}
			f.Set(t, testclaude.Config{Subscription: "team", BridgeSteps: []testclaude.BridgeStep{{Name: tc.tool, Arguments: tc.arguments, Error: true, Contains: tc.contains}}})
			c, _, _ := startInteractiveHost(t, cfg)
			v, err := c.Open(t.Context(), host.Create, "rejected")
			if err != nil {
				t.Fatal(err)
			}
			submitInteractive(t, c, v, "untrusted", "Test the configured permission boundary.")
			end := inspectInteractive(t, c, "rejected", func(v host.View) bool {
				if v.Failure != "" {
					t.Fatal(v.Failure)
				}
				return hasBridgeFinal(v)
			})
			if len(end.Operations) != tc.operations {
				t.Fatal("schema/permission failure did not use the shared lifecycle", len(end.Operations))
			}
			for _, op := range end.Operations {
				if op.Status != operation.StatusFailed {
					t.Fatal("rejected input executed", op.Status)
				}
			}
			if responseCount(end) != 2 {
				t.Fatal("Claude did not continue from safe failure")
			}
			if _, err := os.Stat(filepath.Join(cfg.Runtime.Workspace, "not-authorized.txt")); !os.IsNotExist(err) {
				t.Fatal("denied mutation changed the workspace", err)
			}
		})
	}
}

func TestClaudeBridgeFailedPatchRereadAndSuccessfulEdit(t *testing.T) {
	f := testclaude.New(t)
	cfg := bridgeServeConfiguration(t, f)
	path := filepath.Join(cfg.Runtime.Workspace, "edit.txt")
	if err := os.WriteFile(path, []byte("exact original\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.Set(t, testclaude.Config{Subscription: "team", BridgeSteps: []testclaude.BridgeStep{
		{Name: "read", Arguments: `{"path":"edit.txt"}`, Contains: "exact original"},
		{Name: "edit", Arguments: `{"path":"edit.txt","expected":{"exists":true,"digest":"$REVISION"},"old_text":"absent text","new_text":"must not appear"}`, Error: true, Contains: "no_match"},
		{Name: "read", Arguments: `{"path":"edit.txt"}`, Contains: "exact original"},
		{Name: "edit", Arguments: `{"path":"edit.txt","expected":{"exists":true,"digest":"$REVISION"},"old_text":"exact original","new_text":"fixed after failure"}`, Contains: "applied"},
		{Name: "read", Arguments: `{"path":"edit.txt"}`, Contains: "fixed after failure"},
	}})
	c, _, _ := startInteractiveHost(t, cfg)
	v, err := c.Open(t.Context(), host.Create, "patch-feedback")
	if err != nil {
		t.Fatal(err)
	}
	submitInteractive(t, c, v, "patch-input", "Repair a failed edit from its actual receipt.")
	end := inspectInteractive(t, c, "patch-feedback", func(v host.View) bool {
		if v.Failure != "" {
			t.Fatal(v.Failure)
		}
		return hasBridgeFinal(v)
	})
	noMatch := 0
	for _, op := range end.Operations {
		if op.Status != operation.StatusCompleted {
			t.Fatal("unexpected Operation state", op.ToolName, op.Status)
		}
		if op.ToolName == "edit" {
			state, err := operation.DecodeRemoteJobState(op)
			if err != nil {
				t.Fatal(err)
			}
			if state.TerminalResult == "no_match" {
				noMatch++
			}
		}
	}
	if noMatch != 1 || len(end.Operations) != 5 || responseCount(end) != 6 {
		t.Fatal("failed patch feedback/retry did not preserve canonical work", noMatch)
	}
	content, err := os.ReadFile(path)
	if err != nil || string(content) != "fixed after failure\n" {
		t.Fatal("failed patch mutated the file or the follow-up edit was lost", err)
	}
}

func TestClaudeBridgeCodexSwitchRestoresBothToolCapabilities(t *testing.T) {
	f := testclaude.New(t)
	f.Set(t, testclaude.Config{Subscription: "team", Catalog: hostClaudeCatalog, BridgeSteps: []testclaude.BridgeStep{{Name: "read", Arguments: `{"path":"portable.txt"}`, Contains: "PORTABLE_CONTEXT"}}})
	requests := make(chan multiWire, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var wire multiWire
		if err := json.UnmarshalRead(r.Body, &wire); err != nil {
			t.Error(err)
			return
		}
		requests <- wire
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"type":"response.completed","response":{"id":"codex-bridge-switch","model":"wire-codex","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Codex boundary completed"}]}]}}`+"\n\n")
	}))
	t.Cleanup(server.Close)
	cfg := multiConfiguration(t, f, server.URL, false)
	cfg.Runtime.ClaudeCode.ToolBridge.Enabled = true
	cfg.Permissions.Tools = []string{"read"}
	if err := os.WriteFile(filepath.Join(cfg.Runtime.Workspace, "portable.txt"), []byte("PORTABLE_CONTEXT"), 0600); err != nil {
		t.Fatal(err)
	}
	c, _, _ := startInteractiveHost(t, cfg)
	v, err := c.Open(t.Context(), host.Create, "bridge-provider-switch")
	if err != nil {
		t.Fatal(err)
	}
	providers, err := modelExchange(t.Context(), c.Extension, modelRequest{Action: "model.providers", ID: v.Session.Session.ID})
	if err != nil || len(providers.Providers) != 2 {
		t.Fatal("missing provider registry", err)
	}
	for _, p := range providers.Providers {
		if !p.Tools || p.Availability != "available" {
			t.Fatal("explicit bridge or existing Codex capability unavailable", p.ID)
		}
	}
	submitInteractive(t, c, v, "first-claude", "Read portable.txt using Unreal.")
	inspectInteractive(t, c, v.Session.Session.ID, func(v host.View) bool { return responseCount(v) == 2 })
	multiSelect(t, c, v, 0, "openai-codex", "gpt-6.1-sol", "medium")
	submitInteractive(t, c, v, "codex", "Continue with the portable public receipt.")
	select {
	case wire := <-requests:
		input, _ := json.Marshal(wire.Input)
		if len(wire.Tools) == 0 || wire.Model != "gpt-6.1-sol" || !strings.Contains(string(input), "PORTABLE_CONTEXT") {
			t.Fatal("Codex tool schemas or portable context lost")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no Codex request")
	}
	inspectInteractive(t, c, v.Session.Session.ID, func(v host.View) bool { return responseCount(v) == 3 })
	multiSelect(t, c, v, 1, "claude-code", "discovered-b", "high")
	submitInteractive(t, c, v, "second-claude", "Read portable.txt again using Unreal.")
	end := inspectInteractive(t, c, v.Session.Session.ID, func(v host.View) bool {
		if v.Failure != "" {
			t.Fatal(v.Failure)
		}
		return responseCount(v) == 5
	})
	if len(end.Operations) != 2 || len(f.Calls(t)) != 2 {
		t.Fatal("capability switch lost or duplicated Operations")
	}
	cliArgument(t, f.Calls(t)[1], "--model", "discovered-b")
	cliArgument(t, f.Calls(t)[1], "--effort", "high")
	report := multiReport(t, end)
	if len(report.Turns) != 5 {
		t.Fatal("canonical tool rounds or provider boundary lost", len(report.Turns))
	}
	for i, want := range []string{"claude-code", "claude-code", "openai-codex", "claude-code", "claude-code"} {
		if report.Turns[i].Selection.Provider != want || report.Turns[i].Selection.Revision != []uint64{0, 0, 1, 2, 2}[i] {
			t.Fatal("runtime or prior Turn changed", i)
		}
	}
	if !report.ToolBridgeEnabled || report.ToolCapability != "Unreal tools via SDK MCP" {
		t.Fatal("shared analysis lost the applied capability")
	}
}

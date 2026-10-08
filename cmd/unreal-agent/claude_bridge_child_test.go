//go:build linux || darwin

package main

import (
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/cmd/internal/tui"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudecode"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/subagent"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

func TestClaudeBridgeRequestsBothChildProvidersWithBoundedPermissions(t *testing.T) {
	testClaudeBridgeChildren(t, claudecode.BridgeModeMCP)
}

func TestClaudeStructuredRequestsBothChildProvidersWithBoundedPermissions(t *testing.T) {
	testClaudeBridgeChildren(t, claudecode.BridgeModeStructured)
}

func testClaudeBridgeChildren(t *testing.T, mode string) {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "unreal-agent")
	build := exec.CommandContext(t.Context(), "go", "build", "-race", "-o", binary, ".")
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build child broker: %v %s", err, b)
	}
	f := testclaude.New(t)
	var codexCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		codexCalls.Add(1)
		if !strings.Contains(string(b), "CHILD_GUIDANCE") || !strings.Contains(string(b), "CHILD_BOUND_SNAPSHOT") || strings.Contains(string(b), "CHILD_DISK_CHANGED") {
			t.Error("Codex child did not inherit bound instructions")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"type":"response.completed","response":{"id":"child-r","model":"observed-codex-child","status":"completed","output":[{"type":"function_call","id":"finish","call_id":"finish","name":"Finish","arguments":"{\"status\":\"completed\",\"summary\":\"Codex bridge child completed\",\"changedFiles\":[],\"tests\":[],\"blockers\":[]}"}]}}`+"\n\n")
	}))
	defer server.Close()
	cfg := multiConfiguration(t, f, server.URL, false)
	cfg.Runtime.ClaudeCode.ToolBridge.Enabled = true
	cfg.Runtime.ClaudeCode.ToolBridge.Mode = mode
	cfg.Runtime.ClaudeCode.ManagedPolicyMode = claudecode.ManagedPolicyTrust
	cfg.Permissions.Tools = []string{"SubagentStart", "SubagentSend", "SubagentCancel", "SendParent", "Finish", "read", "Bash"}
	source := cfg.Runtime
	source.SystemPrompt = "CHILD_GUIDANCE"
	cfg.Subagents = map[string]childTemplate{"worker": {
		Runtime:     source,
		Permissions: permission.Config{Tools: []string{"Finish", "read", "Bash"}, ReadRoots: []string{source.Workspace}, NetworkOrigins: []string{server.URL}},
	}}
	if err := os.WriteFile(filepath.Join(source.Workspace, "AGENTS.md"), []byte("CHILD_BOUND_SNAPSHOT"), 0600); err != nil {
		t.Fatal(err)
	}
	f.Set(t, testclaude.Config{Subscription: "team", Catalog: hostClaudeCatalog, BridgeOnce: true,
		BridgeSteps: []testclaude.BridgeStep{
			{Name: "SubagentStart", Arguments: `{"template":"worker","task":"Inspect the Codex child fixture","runtime":{"provider":"openai-codex","model":"gpt-6.1-sol","effort":"medium"}}`, Contains: "Codex bridge child completed", Duplicate: true, DuplicateEnvelope: true},
			{Name: "SubagentStart", Arguments: `{"template":"worker","task":"Check bounded permissions and finish","runtime":{"provider":"claude-code","model":"discovered-b","effort":"high"}}`, Contains: "Claude bridge child completed"},
		},
		BridgeChildSteps: []testclaude.BridgeStep{
			{Name: "read", Arguments: `{"path":"../outside.txt"}`, Error: true, Contains: "read_failed"},
			{Name: "Bash", Arguments: `{"command":"echo forbidden-direct-command","max_output_length":1024}`, Error: true, Contains: "permission denied"},
			{Name: "Finish", Arguments: `{"status":"completed","summary":"Claude bridge child completed","changedFiles":[],"tests":[],"blockers":[]}`},
		},
	})
	directory := privateCLIDirectory(t)
	c, _ := startMultiChildCLI(t, binary, cfg, directory)
	v, err := c.Open(t.Context(), host.Create, "bridge-parent")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(source.Workspace, "AGENTS.md"), []byte("CHILD_DISK_CHANGED"), 0600); err != nil {
		t.Fatal(err)
	}
	submitInteractive(t, c, v, "bridge-child-input", "Ask each child to inspect its fixture.")
	end := inspectInteractive(t, c, "bridge-parent", func(v host.View) bool {
		if v.Failure != "" {
			for _, op := range v.Operations {
				state, _ := operation.DecodeRemoteJobState(op)
				t.Log("canonical child operation", op.ToolName, op.Status, state.TerminalError, state.TerminalResult)
				if plan, err := subagent.DecodePlan(op); err == nil {
					child, e := subagent.ReadChild(t.Context(), filepath.Join(directory, "sessions"), plan.ChildID, 0, 256)
					t.Log("child record", e, child.Finish != nil, child.View.Failure)
					for _, childOp := range child.View.Operations {
						t.Log("child canonical operation", childOp.ToolName, childOp.Status, childOp.Denial)
					}
				}
			}
			t.Log("fake Claude invocation count", len(f.Calls(t)))
			failure, _ := os.ReadFile(filepath.Join(f.Directory, "bridge-failure.json"))
			t.Log("fake protocol expectation", string(failure))
			t.Fatal(v.Failure)
		}
		return hasBridgeFinal(v)
	})
	plans := waitChildPlans(t, c, "bridge-parent", 2)
	if codexCalls.Load() != 1 || len(end.Operations) != 2 {
		t.Fatal("cross-provider request was duplicated or bypassed Operations", codexCalls.Load(), len(end.Operations))
	}
	seen := map[string]bool{}
	rows := []viewer.Row{}
	for _, p := range plans {
		child, err := subagent.ReadChild(t.Context(), filepath.Join(directory, "sessions"), p.ChildID, 0, 256)
		if err != nil || child.Finish == nil {
			t.Fatal("child did not produce canonical completion", err)
		}
		selection := sessionstore.SelectionFromConfiguration(child.Configuration.Runtime)
		seen[selection.Provider] = true
		if child.Configuration.Policy.ProcessMode != permission.ProcessDenied || child.Configuration.Policy.FilesystemUnrestricted || child.Configuration.Policy.NetworkUnrestricted {
			t.Fatal("bridge expanded child permissions")
		}
		if child.Configuration.ProjectInstructions == nil {
			t.Fatal("child did not inherit Project Instructions snapshot")
		}
		if selection.Provider == "claude-code" {
			if selection.Model != "discovered-b" || selection.Effort != "high" || len(child.View.Operations) != 3 {
				t.Fatal("independent child runtime/permission Operations lost", selection, len(child.View.Operations))
			}
			denied, rejectedRead := 0, false
			for _, op := range child.View.Operations {
				if op.ToolName == "read" {
					state, e := operation.DecodeRemoteJobState(op)
					rejectedRead = e == nil && op.Status == operation.StatusFailed && strings.Contains(string(state.Handle), `"code":"read_failed"`)
				}
				if op.Denial != nil {
					denied++
					if op.Status != operation.StatusFailed {
						t.Fatal("denied child tool ran")
					}
				}
			}
			if denied != 1 || !rejectedRead {
				t.Fatal("filesystem/process child boundaries not enforced", denied)
			}
		}
		rows = append(rows, viewer.Row{ID: p.ChildID, ParentID: "bridge-parent", Selection: selection, ParentOperationStatus: operation.StatusCompleted})
	}
	if !seen["openai-codex"] || !seen["claude-code"] {
		t.Fatal("both selected providers not executed", seen)
	}
	topology := tui.BuildOrchestration(tui.Snapshot{ID: "bridge-parent", Operations: end.Operations, Connected: true, Running: true}, viewer.PanelSnapshot{Rows: rows}, time.Now(), 128)
	children := 0
	for _, n := range topology.Nodes {
		if n.Kind == "child_agent" && n.RuntimeKnown {
			children++
		}
	}
	if children != 2 {
		t.Fatal("Orchestration did not project independent child providers", children)
	}
	for _, call := range f.Calls(t) {
		cliArgument(t, call, "--tools", "")
		if strings.Contains(call.System, "CHILD_GUIDANCE") && (!strings.Contains(call.System, "CHILD_BOUND_SNAPSHOT") || strings.Contains(call.System, "CHILD_DISK_CHANGED")) {
			t.Fatal("Claude child reread disk instructions")
		}
		for _, env := range call.Environment {
			if strings.HasPrefix(env, "OPENAI_") || strings.Contains(env, "multi-token-sensitive") {
				t.Fatal("other-provider credential crossed Claude transport")
			}
		}
	}
	b, _ := json.Marshal(topology)
	assertNoCredentialLeak(t, string(b), "multi-token-sensitive", "private-reasoning-sensitive", "token-sensitive")
	stopMultiSession(t, c, v)
}

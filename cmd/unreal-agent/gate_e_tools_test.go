//go:build linux || darwin

package main

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/host/gateway"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudecode"
	"github.com/unreallabsai/unreal-agent/harness/mutation"
	files "github.com/unreallabsai/unreal-agent/harness/native"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

// Fake adapters traverse the real Host/Registry/Permission/Operation/receipt
// path. The model cannot approve Full Access, change the mutation root or
// transform a Bash-created outside directory into a native-tool workspace.
func TestGateECreateEditReadProcessAndWorkspaceBoundaryBothProviders(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			outside := filepath.Join(t.TempDir(), "sample.txt")
			args := func(value any) string {
				data, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				return string(data)
			}
			steps := []testclaude.BridgeStep{
				{Name: "write", Arguments: `{"path":"sample.txt","content":"before\n","expected":{"exists":false}}`, Contains: "applied", Duplicate: true},
				{Name: "read", Arguments: `{"path":"sample.txt"}`, Contains: "before"},
				{Name: "edit", Arguments: args(map[string]any{"path": "sample.txt", "expected": mutation.RevisionOf([]byte("before\n")), "old_text": "before", "new_text": "after"}), Contains: "applied"},
				{Name: "read", Arguments: `{"path":"sample.txt"}`, Contains: "after"},
				{Name: "Bash", Arguments: `{"command":"test \"$(cat sample.txt)\" = after && printf 'GATE_E_FILE_OK\\n'"}`, Contains: "GATE_E_FILE_OK"},
				{Name: "write", Arguments: args(map[string]any{"path": outside, "content": "forbidden", "expected": mutation.Revision{}}), Contains: "workspace_boundary", Error: true},
			}
			var cfg serveConfiguration
			var f *testclaude.Fixture
			var auth string
			var calls atomic.Int32
			if provider == "claude" {
				f = testclaude.New(t)
				cfg = bridgeServeConfiguration(t, f)
				cfg.Runtime.ClaudeCode.ToolBridge.Mode = claudecode.BridgeModeStructured
				f.Set(t, testclaude.Config{Subscription: "team", BridgeManagedPermissionsOnly: true, BridgeSteps: steps})
			} else {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var req multiWire
					if err := json.UnmarshalRead(r.Body, &req); err != nil {
						t.Error(err)
						w.WriteHeader(400)
						return
					}
					n := int(calls.Add(1)) - 1
					if n > len(steps) {
						t.Error("unexpected generation/replay")
						w.WriteHeader(500)
						return
					}
					if n > 0 {
						input, _ := json.Marshal(req.Input)
						if !strings.Contains(string(input), steps[n-1].Contains) {
							t.Error("canonical result did not reach next generation")
						}
					}
					var outputs []any
					if n < len(steps) {
						id := fmt.Sprint("gate-e-", n)
						outputs = []any{map[string]any{"type": "function_call", "id": id, "call_id": id, "name": steps[n].Name, "arguments": steps[n].Arguments}}
					} else {
						outputs = []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "Bridge fixture completed"}}}}
					}
					data, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{"id": fmt.Sprint("response-", n), "status": "completed", "output": outputs}})
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprintf(w, "data: %s\n\n", data)
				}))
				t.Cleanup(server.Close)
				cfg = codexServeConfiguration(t.TempDir(), server.URL)
				auth = filepath.Join(t.TempDir(), "auth.json")
				writeCodexAuth(t, auth, "gate-e-fake-secret", "gate-e-fake-account")
			}
			cfg = humanModeConfiguration(cfg)
			var c *gateway.Client
			if auth != "" {
				c, _, _ = startInteractiveHost(t, cfg, "--codex-auth-file", auth)
			} else {
				c, _, _ = startInteractiveHost(t, cfg)
			}
			v, err := c.Open(t.Context(), host.Create, "gate-e")
			if err != nil {
				t.Fatal(err)
			}
			control, err := c.WatchPermissions(t.Context(), "gate-e", v.Generation)
			if err != nil {
				t.Fatal(err)
			}
			defer control.Cancel()
			setGatewayHumanMode(t, control, permission.FullAccess)
			submitInteractive(t, c, v, "task", "Create, observe the revision, edit, reread, test, and report the outside-workspace failure.")
			end := inspectInteractive(t, c, "gate-e", func(v host.View) bool {
				if v.Failure != "" {
					t.Fatal(v.Failure)
				}
				return hasBridgeFinal(v)
			})
			if len(end.Operations) != len(steps) {
				t.Fatalf("canonical Operation count %d, want %d (duplicate write must execute once)", len(end.Operations), len(steps))
			}
			seen := map[operation.ID]bool{}
			outsideFailures := 0
			for _, op := range end.Operations {
				if seen[op.ID] {
					t.Fatal("duplicate Operation")
				}
				seen[op.ID] = true
				if op.ToolName == "Bash" {
					var state operation.ShellState
					if err := json.Unmarshal(op.State, &state); err != nil {
						t.Fatal(err)
					}
					if op.Status != operation.StatusCompleted || state.Result == nil || state.Result.ExitCode != 0 {
						t.Fatal("Bash disagrees with file receipts", op)
					}
					continue
				}
				state, err := operation.DecodeRemoteJobState(op)
				if err != nil {
					t.Fatal(err)
				}
				var result files.Result
				if err := json.Unmarshal(state.Handle, &result); err != nil {
					t.Fatal(err)
				}
				if result.Reason == mutation.ReasonWorkspaceBoundary {
					outsideFailures++
					if op.Status != operation.StatusFailed || op.Denial != nil || result.Code != "invalid" || result.Mutation == nil || result.Mutation.Code != mutation.Invalid {
						t.Fatal("invalid boundary misclassified/allowed", op)
					}
				} else if op.Status != operation.StatusCompleted {
					t.Fatal("in-workspace create/edit/read failed", op)
				}
				if op.ToolName == "write" || op.ToolName == "edit" {
					if op.Permission == nil || op.Permission.State != "full_access" {
						t.Fatal("missing canonical human decision")
					}
				}
			}
			if outsideFailures != 1 {
				t.Fatal("missing exact boundary receipt")
			}
			data, err := os.ReadFile(filepath.Join(cfg.Runtime.Workspace, "sample.txt"))
			if err != nil || string(data) != "after\n" {
				t.Fatal("actual content differs from receipts", err)
			}
			if _, err := os.Stat(outside); !os.IsNotExist(err) {
				t.Fatal("outside write side effect")
			}
			if f != nil {
				for _, call := range f.Calls(t) {
					cliArgument(t, call, "--tools", "")
					cliArgument(t, call, "--allowedTools", "StructuredOutput")
					cliArgument(t, call, "--mcp-config", `{"mcpServers":{}}`)
				}
			}
		})
	}
}

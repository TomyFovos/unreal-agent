//go:build linux || darwin

package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/host/gateway"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/subagent"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

func startMultiChildCLI(t *testing.T, binary string, cfg serveConfiguration, directory string) (*gateway.Client, func()) {
	t.Helper()
	raw, e := json.Marshal(cfg)
	if e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(directory, "config.json")
	if e = os.WriteFile(path, raw, 0600); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, binary, "serve", "--config", path, "--session-directory", filepath.Join(directory, "sessions"), "--socket", filepath.Join(directory, "host.sock"))
	var diagnostics credentialOutput
	cmd.Stdout = &diagnostics
	cmd.Stderr = &diagnostics
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	done := make(chan struct{})
	var failure error
	go func() { failure = cmd.Wait(); close(done) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-done:
				if failure != nil {
					t.Errorf("fake CLI Host: %v: %s", failure, diagnostics.String())
				}
			case <-time.After(5 * time.Second):
				t.Error("child Host did not drain")
			}
		})
	}
	t.Cleanup(stop)
	client := gateway.NewClient(filepath.Join(directory, "host.sock"))
	t.Cleanup(func() { client.Close() })
	waitInteractive(t, func() bool {
		select {
		case <-done:
			t.Fatalf("CLI startup: %v: %s", failure, diagnostics.String())
		default:
		}
		_, e := client.Methods(t.Context())
		return e == nil
	})
	return client, stop
}
func childStart(t *testing.T, c *gateway.Client, v host.View, id, name, task string, runtime *subagent.RuntimeRequest) host.Receipt {
	t.Helper()
	out, e := modelExchange(t.Context(), c.Extension, modelRequest{Action: "child.start", ID: v.Session.Session.ID, Generation: v.Generation, InputID: inbox.ID(id), Template: name, Task: task, ChildRuntime: runtime})
	if e != nil || out.Receipt == nil {
		t.Fatal("child start", e)
	}
	return *out.Receipt
}
func waitChildPlans(t *testing.T, c *gateway.Client, id session.ID, count int) []subagent.Plan {
	t.Helper()
	var plans []subagent.Plan
	inspectInteractive(t, c, id, func(v host.View) bool {
		plans = nil
		for _, op := range v.Operations {
			p, e := subagent.DecodePlan(op)
			if e == nil && p.Action == "start" {
				if op.Status == operation.StatusFailed || op.Status == operation.StatusCanceled {
					t.Fatalf("child start reached %s instead of completion", op.Status)
				}
				if op.Status != operation.StatusCompleted {
					return false
				}
				plans = append(plans, p)
			}
		}
		return len(plans) == count
	})
	return plans
}

func TestMultiProviderConcurrentChildrenBothDirectionsAndRecovery(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "unreal-agent")
	build := exec.CommandContext(t.Context(), "go", "build", "-race", "-o", binary, ".")
	if b, e := build.CombinedOutput(); e != nil {
		t.Fatalf("build fake-process broker: %v %s", e, b)
	}
	for _, codexFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("codex-parent-%t", codexFirst), func(t *testing.T) {
			f := testclaude.New(t)
			gate := filepath.Join(f.Directory, "release")
			f.Set(t, testclaude.Config{Subscription: "team", Catalog: hostClaudeCatalog, Gate: gate})
			var codexChildren atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, e := io.ReadAll(r.Body)
				if e != nil {
					t.Errorf("fake backend body read: %v (request canceled=%t, content length=%d)", e, r.Context().Err() != nil, r.ContentLength)
					return
				}
				if r.Header.Get("Authorization") != "Bearer multi-token-sensitive" {
					t.Error("child lost Codex external auth")
				}
				output := []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "parent fixture"}}}}
				if bytes.Contains(body, []byte("CHILD_GUIDANCE")) {
					codexChildren.Add(1)
					if !bytes.Contains(body, []byte("BOUND_INSTRUCTIONS_A")) || bytes.Contains(body, []byte("DISK_INSTRUCTIONS_B")) || bytes.Contains(body, []byte("PARENT_PRIVATE_CONTEXT")) {
						t.Error("child context crossed snapshot boundary")
					}
					output = []any{map[string]any{"type": "function_call", "id": "finish", "call_id": "finish", "name": "Finish", "arguments": `{"status":"completed","summary":"Codex child fixture completed","changedFiles":[],"tests":[],"blockers":[]}`}}
				}
				b, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{"id": "fake-r", "model": "codex-wire", "status": "completed", "output": output, "usage": map[string]any{"input_tokens": 12, "output_tokens": 3}}})
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: %s\n\n", b)
			}))
			defer server.Close()
			cfg := multiConfiguration(t, f, server.URL, codexFirst)
			cfg.Permissions.Tools = []string{"SubagentStart", "SubagentSend", "SubagentCancel", "SendParent", "Finish", "read"}
			source := cfg.Runtime
			source.SystemPrompt = "CHILD_GUIDANCE"
			cfg.Subagents = map[string]childTemplate{"worker": {Runtime: source, Permissions: permission.Config{Tools: []string{"Finish", "SendParent", "read"}, ReadRoots: []string{source.Workspace}, NetworkOrigins: []string{server.URL}}}}
			if e := os.WriteFile(filepath.Join(source.Workspace, "AGENTS.md"), []byte("BOUND_INSTRUCTIONS_A"), 0600); e != nil {
				t.Fatal(e)
			}
			directory := privateCLIDirectory(t)
			c, stop := startMultiChildCLI(t, binary, cfg, directory)
			parent, e := c.Open(t.Context(), host.Create, "cross-parent")
			if e != nil {
				t.Fatal(e)
			}
			if e = os.WriteFile(filepath.Join(source.Workspace, "AGENTS.md"), []byte("DISK_INSTRUCTIONS_B"), 0600); e != nil {
				t.Fatal(e)
			}
			requests := []*subagent.RuntimeRequest{{Provider: "claude-code", Model: "claude-configured-a", Effort: "medium"}, {Provider: "openai-codex", Model: "gpt-6.1-sol", Effort: "high"}, {Provider: "claude-code", Model: "discovered-b", Effort: "high"}}
			// Invalid runtime requests are rejected before an input/Operation is added.
			for i, r := range []*subagent.RuntimeRequest{{Provider: "unknown", Model: "m", Effort: "medium"}, {Provider: "claude-code", Model: "invented", Effort: "high"}, {Provider: "claude-code", Model: "discovered-b", Effort: "low"}} {
				_, e = modelExchange(t.Context(), c.Extension, modelRequest{Action: "child.start", ID: "cross-parent", Generation: parent.Generation, InputID: inbox.ID(fmt.Sprint("bad-", i)), Template: "worker", Task: "bad task", ChildRuntime: r})
				if e == nil {
					t.Fatal("unvalidated child runtime")
				}
			}
			before, _ := c.Inspect(t.Context(), "cross-parent", 0, 256)
			if len(before.Operations) != 0 {
				t.Fatal("invalid runtime created canonical operation")
			}
			for i, r := range requests {
				childStart(t, c, parent, fmt.Sprintf("start-%d", i), "worker", fmt.Sprintf("TASK_%d", i), r)
			}
			// Omitted runtime is a creation-time copy of the parent's applied choice.
			firstReceipt := childStart(t, c, parent, "inherited", "worker", "INHERITED_TASK", nil)
			waiting := inspectInteractive(t, c, "cross-parent", func(v host.View) bool { return len(v.Operations) == 4 })
			var inheritedProvider, inheritedModel string
			for _, op := range waiting.Operations {
				p, e := subagent.DecodePlan(op)
				if e == nil && p.Text == "INHERITED_TASK" {
					s := sessionstore.SelectionFromConfiguration(p.Configuration.Runtime)
					inheritedProvider, inheritedModel = s.Provider, s.Model
				}
			}
			if inheritedProvider != cfg.Runtime.Provider.Provider || inheritedModel != cfg.Runtime.Provider.Model.ID {
				t.Fatal("omitted child did not inherit parent")
			}
			// Queue a provider switch while children run, then retry the same child start.
			nextProvider, nextModel, nextEffort := "openai-codex", "gpt-6.1-sol", llm.ReasoningEffort("medium")
			if codexFirst {
				nextProvider, nextModel, nextEffort = "claude-code", "discovered-b", "high"
			}
			multiSelect(t, c, parent, 0, nextProvider, nextModel, nextEffort)
			again := childStart(t, c, parent, "inherited", "worker", "INHERITED_TASK", nil)
			if again.Sequence != firstReceipt.Sequence {
				t.Fatal("retry changed child runtime/created new input")
			}
			if e = os.WriteFile(gate, []byte("ready"), 0600); e != nil {
				t.Fatal(e)
			}
			plans := waitChildPlans(t, c, "cross-parent", 4)
			if codexChildren.Load() == 0 {
				t.Fatal("no cross-provider Codex child")
			}
			for _, p := range plans {
				v, e := subagent.ReadChild(t.Context(), filepath.Join(directory, "sessions"), p.ChildID, 0, 256)
				if e != nil || v.Finish == nil {
					t.Fatal("canonical child completion", e)
				}
				runtime := sessionstore.SelectionFromConfiguration(v.Configuration.Runtime)
				original := sessionstore.SelectionFromConfiguration(p.Configuration.Runtime)
				if runtime.Provider != original.Provider || runtime.Model != original.Model || runtime.Effort != original.Effort {
					t.Fatal("child identity followed parent")
				}
				if runtime.Provider == "claude-code" {
					if len(v.View.Operations) != 0 || v.Finish.ModelTurnID == "" || v.Finish.OperationID != "" {
						t.Fatal("Claude child created a direct/synthetic Tool Operation")
					}
					if v.Configuration.Policy.ProcessMode != permission.ProcessDenied {
						t.Fatal("provider transport widened child tool permission")
					}
				}
			}
			for _, call := range f.Calls(t) {
				cliArgument(t, call, "--tools", "")
				cliArgument(t, call, "--disallowedTools", "*")
				cliArgument(t, call, "--mcp-config", `{"mcpServers":{}}`)
				if strings.Contains(call.System, "CHILD_GUIDANCE") {
					if !strings.Contains(call.System, "BOUND_INSTRUCTIONS_A") || strings.Contains(call.System, "DISK_INSTRUCTIONS_B") {
						t.Fatal("Claude child reread Project Instructions")
					}
					for _, env := range call.Environment {
						if strings.HasPrefix(env, "ANTHROPIC_API_KEY=") || strings.HasPrefix(env, "OTEL_") || strings.HasPrefix(env, "OPENAI_CODEX_ACCESS_TOKEN=") {
							t.Fatal("child inherited parent secrets")
						}
					}
				}
			}
			parentState, _ := c.Inspect(t.Context(), "cross-parent", 0, 256)
			model := viewer.New(viewer.SubagentOptions())
			if e = model.Replace("cross-parent", parentState); e != nil {
				t.Fatal(e)
			}
			rows := model.Rows(time.Now())
			if len(rows) != 5 {
				t.Fatal("child projection missing")
			}
			for _, r := range rows {
				if r.ParentID != "" && (r.Selection == nil || r.Selection.Provider == "" || r.Selection.Model == "") {
					t.Fatal("future orchestration runtime metadata missing")
				}
			}
			if rendered := viewer.RenderRows(rows, ""); !strings.Contains(rendered, "claude-code") || !strings.Contains(rendered, "openai-codex") {
				t.Fatal("children hide runtime")
			}
			// Interrupt a real child subprocess before its response is committed.
			// Parent recovery must use the recorded child's own runtime, even though
			// a different parent provider selection has been queued meanwhile.
			interruptedGate := filepath.Join(f.Directory, "resume-release")
			f.Set(t, testclaude.Config{Subscription: "team", Catalog: hostClaudeCatalog, Gate: interruptedGate})
			childStart(t, c, parent, "interrupted", "worker", "INTERRUPTED_TASK", &subagent.RuntimeRequest{Provider: "claude-code", Model: "discovered-b", Effort: "high"})
			var interruptedID session.ID
			inspectInteractive(t, c, "cross-parent", func(v host.View) bool {
				for _, op := range v.Operations {
					p, e := subagent.DecodePlan(op)
					if e == nil && p.Text == "INTERRUPTED_TASK" {
						interruptedID = p.ChildID
						return op.Status == operation.StatusAwaiting
					}
				}
				return false
			})
			waitInteractive(t, func() bool {
				for _, call := range f.Calls(t) {
					if strings.Contains(call.Input, "INTERRUPTED_TASK") {
						return true
					}
				}
				return false
			})
			stop()
			beforeRestart, e := subagent.ReadChild(t.Context(), filepath.Join(directory, "sessions"), interruptedID, 0, 256)
			if e != nil || beforeRestart.Finish != nil {
				t.Fatal("interrupted child falsely finished", e)
			}
			if e = os.WriteFile(interruptedGate, []byte("ready"), 0600); e != nil {
				t.Fatal(e)
			}
			c2, _ := startMultiChildCLI(t, binary, cfg, directory)
			resumed, e := c2.Open(t.Context(), host.Resume, "cross-parent")
			if e != nil {
				t.Fatal(e)
			}
			recovered := waitChildPlans(t, c2, "cross-parent", 5)
			for _, p := range recovered {
				v, e := subagent.ReadChild(t.Context(), filepath.Join(directory, "sessions"), p.ChildID, 0, 256)
				if e != nil || v.Finish == nil {
					t.Fatal("child result lost on parent resume", e)
				}
				// Every actual child process uses the common context engine. Its
				// derived request identity follows its own immutable runtime, not
				// the switched parent's current provider/model/effort.
				data, e := os.ReadFile(filepath.Join(directory, "sessions", contextengine.DerivedDirectory, string(p.ChildID), contextengine.DerivedFile))
				if e != nil {
					t.Fatal("child context engine did not write metadata", e)
				}
				var manifest contextengine.Manifest
				if json.Unmarshal(data, &manifest) != nil || manifest.Diagnostics == nil {
					t.Fatal("missing child context diagnostics")
				}
				selection := sessionstore.SelectionFromConfiguration(v.Configuration.Runtime)
				d := manifest.Diagnostics
				if d.Version != 1 || d.Provider != selection.Provider || d.Model != selection.Model || d.Effort != string(selection.Effort) || d.EstimatedInputTokens > d.Budget.Input {
					t.Fatal("cross-provider child context used parent identity", d)
				}
				if strings.Contains(string(data), p.Text) || strings.Contains(string(data), "multi-account-sensitive") || strings.Contains(string(data), "multi-token-sensitive") {
					t.Fatal("child derived context leaked task/credentials")
				}
				if p.ChildID == interruptedID {
					s := sessionstore.SelectionFromConfiguration(v.Configuration.Runtime)
					if s.Provider != "claude-code" || s.Model != "discovered-b" || s.Effort != "high" {
						t.Fatal("child resume inherited changed parent")
					}
				}
			}
			_ = resumed

		})
	}
}

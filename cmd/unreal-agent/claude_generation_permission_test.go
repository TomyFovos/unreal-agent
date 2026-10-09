//go:build linux || darwin

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudecode"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

func TestClaudeStructuredRefreshesPermissionsEveryGeneration(t *testing.T) {
	f := testclaude.New(t)
	cfg := humanModeConfiguration(bridgeServeConfiguration(t, f))
	cfg.Runtime.ClaudeCode.ToolBridge.Mode = claudecode.BridgeModeStructured
	if err := os.WriteFile(filepath.Join(cfg.Runtime.Workspace, "source.txt"), []byte("synthetic input\n"), 0600); err != nil {
		t.Fatal(err)
	}
	script := []string{
		`{"type":"action","action":{"id":"read-1","tool":"read","arguments":{"path":"source.txt"}}}`,
		`{"type":"action","action":{"id":"write-1","tool":"write","arguments":{"path":"result.txt","content":"observed\n","expected":{"exists":false}}}}`,
		`{"type":"action","action":{"id":"write-1","tool":"write","arguments":{"path":"result.txt","content":"observed\n","expected":{"exists":false}}}}`,
		`{"type":"action","action":{"id":"write-2","tool":"write","arguments":{"path":"result.txt","content":"observed\n","expected":{"exists":false}}}}`,
		`{"type":"final","final":{"message":"Bridge fixture completed"}}`,
	}
	gates := []string{filepath.Join(t.TempDir(), "first"), filepath.Join(t.TempDir(), "second"), filepath.Join(t.TempDir(), "third")}
	setup := func(gate string) {
		f.Set(t, testclaude.Config{Subscription: "team", Gate: gate, StructuredResponses: script})
	}
	setup(gates[0])
	client, _, _ := startInteractiveHost(t, cfg)
	v, err := client.Open(t.Context(), host.Create, "generation-permissions")
	if err != nil {
		t.Fatal(err)
	}
	control, err := client.WatchPermissions(t.Context(), "generation-permissions", v.Generation)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Cancel()
	submitInteractive(t, client, v, "task", "Read, request a write, replay the same request, then request a new write and finish.")
	for generation, mode := range []permission.Mode{permission.FullAccess, permission.ReadOnly, permission.FullAccess} {
		waitInteractive(t, func() bool { return len(f.Calls(t)) >= generation+1 })
		calls := f.Calls(t)
		want := []string{"Read Only", "Full Access", "Read Only"}[generation]
		if strings.Count(calls[generation].System, "Unreal permission mode:") != 1 || !strings.Contains(calls[generation].System, "Unreal permission mode: "+want+".") {
			t.Fatal("inner generation used stale or contradictory permission layer")
		}
		current, err := client.Inspect(t.Context(), "generation-permissions", 0, 256)
		if err != nil {
			t.Fatal(err)
		}
		control.Initial = current.Permissions
		if generation == 0 {
			choice := sessionstore.RuntimeSelection{Version: 1, RequestID: "deferred-model", Model: "claude-configured-b", Effort: "high"}
			if _, err := modelExchange(t.Context(), client.Extension, modelRequest{Action: "model.select", ID: "generation-permissions", Generation: v.Generation, Selection: choice}); err != nil {
				t.Fatal(err)
			}
		}
		setGatewayHumanMode(t, control, mode)
		if generation+1 < len(gates) {
			setup(gates[generation+1])
		} else {
			setup("")
		}
		if err := os.WriteFile(gates[generation], []byte("release"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	end := inspectInteractive(t, client, "generation-permissions", func(v host.View) bool {
		if v.Failure != "" {
			t.Fatal(v.Failure)
		}
		return hasBridgeFinal(v)
	})
	if len(end.Operations) != 3 {
		t.Fatal("permission revision changed replay identity or duplicated a side effect")
	}
	for _, call := range f.Calls(t) {
		cliArgument(t, call, "--model", "claude-configured-a")
		cliArgument(t, call, "--effort", "medium")
	}
	for _, item := range end.History.Items {
		if r, ok := item.Data.(sessionstore.HostRecord); ok && r.Kind == sessionstore.HostRuntimeApplied {
			t.Fatal("permission refresh applied pending runtime mid-generation")
		}
	}
	denied, allowed := 0, 0
	for _, op := range end.Operations {
		if op.ToolName == "write" {
			if op.Denial != nil {
				denied++
			} else if op.Status == operation.StatusCompleted {
				allowed++
			}
		}
	}
	if denied != 1 || allowed != 1 {
		t.Fatal("past denial was rewritten or new action did not use current permission")
	}
	content, err := os.ReadFile(filepath.Join(cfg.Runtime.Workspace, "result.txt"))
	if err != nil || string(content) != "observed\n" {
		t.Fatal("file disagrees with canonical receipts")
	}
}

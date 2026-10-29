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
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
)

func humanModeConfiguration(cfg serveConfiguration) serveConfiguration {
	// The operator ceiling does not grant a mode or approve an Operation.
	cfg.Permissions = permission.Config{Tools: []string{"read", "grep", "glob"}, ReadRoots: []string{cfg.Runtime.Workspace}, NetworkOrigins: cfg.Permissions.NetworkOrigins}
	cfg.PermissionModes = &permission.ModesConfig{Enabled: true, Maximum: &permission.Config{Tools: []string{"read", "grep", "glob", "write", "edit", "Bash"}, FilesystemUnrestricted: true, NetworkUnrestricted: true, ProcessMode: permission.ProcessUnrestricted}}
	return cfg
}

func setGatewayHumanMode(t *testing.T, w permission.ControlConnection, mode permission.Mode) {
	t.Helper()
	s := w.Initial
	if _, err := w.Request(t.Context(), permission.ControlRequest{Action: "set_mode", Expected: s.Revision, Mode: mode}); err != nil {
		t.Fatal(err)
	}
}

func TestHumanProviderPermissionsUseCanonicalOperationsForBothAdapters(t *testing.T) {
	for _, provider := range []string{"codex"} {
		for _, mode := range []string{"readonly", "ask-allow", "ask-deny", "full"} {
			t.Run(provider+"/"+mode, func(t *testing.T) {
				allow := mode == "ask-allow" || mode == "full"
				args := `{"path":"human.txt","content":"human-approved\n","expected":{"exists":false}}`
				var cfg serveConfiguration
				var auth string
				var calls atomic.Int32
				var receipt atomic.Bool
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var req multiWire
					if err := json.UnmarshalRead(r.Body, &req); err != nil {
						t.Error(err)
						return
					}
					var outputs []any
					if calls.Add(1) == 1 {
						outputs = []any{map[string]any{"type": "function_call", "id": "human-call", "call_id": "human-call", "name": "write", "arguments": args}}
					} else {
						input, _ := json.Marshal(req.Input)
						contains := "permission denied"
						if allow {
							contains = "applied"
						}
						receipt.Store(strings.Contains(string(input), contains))
						outputs = []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "Bridge fixture completed"}}}}
					}
					event, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{"id": "human-response", "status": "completed", "output": outputs}})
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprintf(w, "data: %s\n\n", event)
				}))
				t.Cleanup(server.Close)
				cfg = codexServeConfiguration(t.TempDir(), server.URL)
				auth = filepath.Join(t.TempDir(), "auth.json")
				writeCodexAuth(t, auth, "human-fixture-secret", "human-fixture-account")

				cfg = humanModeConfiguration(cfg)
				var c *gateway.Client
				if auth != "" {
					c, _, _ = startInteractiveHost(t, cfg, "--codex-auth-file", auth)
				} else {
					c, _, _ = startInteractiveHost(t, cfg)
				}
				v, err := c.Open(t.Context(), host.Create, "human-provider")
				if err != nil {
					t.Fatal(err)
				}
				if v.Permissions.Mode != permission.ReadOnly {
					t.Fatal("config granted elevation")
				}
				w, err := c.WatchPermissions(t.Context(), "human-provider", v.Generation)
				if err != nil {
					t.Fatal(err)
				}
				defer w.Cancel()
				if strings.HasPrefix(mode, "ask") {
					setGatewayHumanMode(t, w, permission.Ask)
				}
				if mode == "full" {
					setGatewayHumanMode(t, w, permission.FullAccess)
				}
				submitInteractive(t, c, v, "task", "Run the fake write proposal and report its actual receipt.")
				if strings.HasPrefix(mode, "ask") {
					pending := inspectInteractive(t, c, "human-provider", func(v host.View) bool { return len(v.Permissions.Pending) == 1 })
					if _, err := os.Stat(filepath.Join(cfg.Runtime.Workspace, "human.txt")); !os.IsNotExist(err) {
						t.Fatal("effect before human approval")
					}
					if _, err := w.Request(t.Context(), permission.ControlRequest{Action: "decide", Expected: pending.Permissions.Revision, Operation: pending.Permissions.Pending[0].ID, Allow: allow}); err != nil {
						t.Fatal(err)
					}
				}
				end := inspectInteractive(t, c, "human-provider", func(v host.View) bool {
					if v.Failure != "" {
						t.Fatal(v.Failure)
					}
					return hasBridgeFinal(v)
				})
				if len(end.Operations) != 1 || end.Operations[0].Permission == nil {
					t.Fatal("missing/duplicated canonical Operation")
				}
				op := end.Operations[0]
				if allow {
					if op.Status != operation.StatusCompleted || op.Denial != nil {
						t.Fatal(op)
					}
				} else if op.Status != operation.StatusFailed || op.Denial == nil {
					t.Fatal(op)
				}
				content, err := os.ReadFile(filepath.Join(cfg.Runtime.Workspace, "human.txt"))
				if allow && (err != nil || string(content) != "human-approved\n") || !allow && !os.IsNotExist(err) {
					t.Fatal("unexpected effect", err)
				}
				if calls.Load() != 2 || !receipt.Load() {
					t.Fatal("Codex did not receive canonical permission receipt")
				}
				encoded, _ := json.Marshal(end.History)
				if strings.Contains(string(encoded), "Challenge") || strings.Contains(string(encoded), "Lease") || strings.Contains(string(encoded), "human-fixture-secret") {
					t.Fatal("private authority/credential entered history")
				}
			})
		}
	}
}

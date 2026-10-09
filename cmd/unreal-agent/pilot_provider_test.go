//go:build linux || darwin

package main

import (
	"context"
	"encoding/json/v2"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/host/gateway"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudecode"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

func pilotConfiguration(t *testing.T, sandbox, workspace, binary string) serveConfiguration {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	code := `import sys,json; sys.path.insert(0,sys.argv[1]); import pilot_provider as p; print(json.dumps(p.configuration(sys.argv[2],sys.argv[3],"claude-configured-a" if sys.argv[2]=="devlore" else "gpt-6.1-sol","medium",sys.argv[4],sys.argv[2]=="devlore",sys.argv[2]=="devlore")))`
	cmd := exec.Command("python3", "-c", code, filepath.Join(root, "scripts"), sandbox, workspace, binary)
	cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	raw, err := cmd.Output()
	if err != nil {
		t.Fatal("pilot configuration generator failed", err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := readServeConfiguration(path)
	if err != nil {
		t.Fatal("generated config does not match the native strict schema", err)
	}
	return cfg
}

func TestPilotProviderConfigurationMatchesNativeSchema(t *testing.T) {
	for _, sandbox := range []string{"asp", "devlore"} {
		t.Run(sandbox, func(t *testing.T) {
			cfg := pilotConfiguration(t, sandbox, t.TempDir(), "/unused/claude")
			if cfg.Launcher == nil || cfg.Launcher.DiscoverProviders == nil || *cfg.Launcher.DiscoverProviders || len(cfg.Providers) != 0 {
				t.Fatal("pilot permits additional provider discovery")
			}
			registry, err := registeredProviders(cfg.Runtime, cfg.Providers)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = registry.Resolve(cfg.Runtime.Provider); err != nil {
				t.Fatal("invalid provider/model registration", err)
			}
			p, err := permission.New(cfg.Permissions)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			for _, tool := range []string{"read", "grep", "glob"} {
				if p.CheckTool(tool) != nil {
					t.Fatal("read-only tool denied", tool)
				}
			}
			for _, tool := range []string{"write", "edit", "Bash", "SubagentStart"} {
				if p.CheckTool(tool) == nil {
					t.Fatal("pilot widened tool permission", tool)
				}
			}
			if p.CheckProcess() == nil {
				t.Fatal("pilot permits tool subprocesses")
			}
			inside := filepath.Join(cfg.Runtime.Workspace, "sample.txt")
			outside := filepath.Join(t.TempDir(), "outside.txt")
			for _, path := range []string{inside, outside} {
				if err := os.WriteFile(path, []byte("dummy"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if p.CheckPath(inside, false) != nil || p.CheckPath(inside, true) == nil || p.CheckPath(outside, false) == nil {
				t.Fatal("pilot does not enforce its read-only workspace boundary")
			}
			if sandbox == "devlore" {
				if cfg.Runtime.ClaudeCode.ToolBridge.ResolvedMode() != claudecode.BridgeModeStructured || cfg.Runtime.ClaudeCode.ManagedPolicyMode != claudecode.ManagedPolicyTrust {
					t.Fatal("incorrect explicit Claude bridge/policy configuration")
				}
				if _, err := claudecode.NewClient(*cfg.Runtime.ClaudeCode); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestPilotProviderPackagedConfigurationStartup(t *testing.T) {
	kit := os.Getenv("UNREAL_PILOT_KIT_DIRECTORY")
	if kit == "" {
		t.Skip("set UNREAL_PILOT_KIT_DIRECTORY to the existing verified 6284e49 kit")
	}
	for _, sandbox := range []string{"asp", "devlore"} {
		t.Run(sandbox, func(t *testing.T) {
			f := testclaude.New(t)
			f.Set(t, testclaude.Config{Subscription: "team", BridgeManagedPermissionsOnly: true})
			cfg := pilotConfiguration(t, sandbox, f.Home, f.Binary)
			directory := privateCLIDirectory(t)
			path := filepath.Join(directory, "config.json")
			raw, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			socket := filepath.Join(directory, "host.sock")
			args := []string{"serve", "--config", path, "--session-directory", filepath.Join(directory, "sessions"), "--socket", socket}
			if sandbox == "asp" {
				auth := filepath.Join(directory, "auth.json")
				writeCodexAuth(t, auth, "dummy-token", "dummy-account")
				args = append(args, "--codex-auth-file", auth)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			process := exec.CommandContext(ctx, filepath.Join(kit, "unreal-agent"), args...)
			process.Env = append(os.Environ(), "HOME="+f.Home)
			var output credentialOutput
			process.Stdout, process.Stderr = &output, &output
			if err := process.Start(); err != nil {
				cancel()
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- process.Wait() }()
			t.Cleanup(func() {
				defer cancel()
				_ = process.Process.Signal(syscall.SIGTERM)
				select {
				case err := <-done:
					if err != nil {
						t.Error("packaged Pilot Host failed", err, output.String())
					}
				case <-time.After(5 * time.Second):
					_ = process.Process.Kill()
					<-done
					t.Error("packaged Pilot Host did not drain")
				}
			})
			client := gateway.NewClient(socket)
			defer client.Close()
			waitInteractive(t, func() bool { return probeHost(t.Context(), client) == nil })
			view, err := client.Open(t.Context(), "create", "pilot-no-inference")
			if err != nil || !view.Running || len(view.Operations) != 0 || responseCount(view) != 0 || len(f.Calls(t)) != 0 {
				t.Fatal("packaged Pilot startup/idle Session failed or initiated inference", err)
			}
		})
	}
}

func TestPilotProviderScriptSmokeThroughFakeClaudeHost(t *testing.T) {
	f := testclaude.New(t)
	workspace := filepath.Join(f.Home, "workspace")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := pilotConfiguration(t, "devlore", workspace, f.Binary)
	f.Set(t, testclaude.Config{Subscription: "team", BridgeManagedPermissionsOnly: true, StructuredResponses: []string{`{"type":"final","final":{"message":"Unreal devlore provider smoke OK."}}`}})
	client, store, _ := startInteractiveHost(t, cfg)
	directory := filepath.Dir(store)
	pointer := filepath.Join(directory, "latest-smoke.json")
	root, _ := filepath.Abs(filepath.Join("..", ".."))
	// The in-process Host advertises this exact test executable. The Python
	// compatibility client must prove the same build, not only matching paths.
	kit := filepath.Join(directory, "synthetic-kit")
	if err := os.Mkdir(kit, 0700); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	image, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kit, "unreal-agent"), image, 0700); err != nil {
		t.Fatal(err)
	}
	// Exercise the actual Python control-plane requests and binding check against
	// the native Host, with a fake Claude CLI and no network/backend inference.
	code := `import sys; from pathlib import Path; sys.path.insert(0,sys.argv[1]); import pilot_provider as p; d=Path(sys.argv[2]); paths={"home":Path(sys.argv[3]),"config":d/"config.json","state":d,"sessions":d/"sessions","socket":d/"host.sock"}; gateway=p.ensure_host(Path(sys.argv[4]),paths,"",{}); p.smoke(gateway,"devlore",d/"latest-smoke.json",timeout=30)`
	cmd := exec.CommandContext(t.Context(), "python3", "-c", code, filepath.Join(root, "scripts"), directory, f.Home, kit)
	cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("pilot script/native Host integration failed: %v %s", err, output)
	}
	var record struct {
		Session string `json:"session"`
	}
	raw, err := os.ReadFile(pointer)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	view, err := client.Inspect(t.Context(), session.ID(record.Session), 0, 64)
	if err != nil || responseCount(view) != 1 || len(view.Operations) != 0 || len(f.Calls(t)) != 1 {
		t.Fatal("pilot smoke did not preserve exactly one durable response without operations", err)
	}
	call := f.Calls(t)[0]
	cliArgument(t, call, "--tools", "")
	cliArgument(t, call, "--allowedTools", "StructuredOutput")
	cliArgument(t, call, "--mcp-config", `{"mcpServers":{}}`)
}

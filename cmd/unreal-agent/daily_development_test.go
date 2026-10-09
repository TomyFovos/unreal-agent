//go:build linux || darwin

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/permission"
)

func TestDailyDevelopmentActualNativeConfigurationAndHumanCeiling(t *testing.T) {
	for _, sandbox := range []string{"asp", "devlore"} {
		t.Run(sandbox, func(t *testing.T) {
			workspace := t.TempDir()
			cfg := dailyDevelopmentConfiguration(t, sandbox, workspace, "/unused/claude")
			base, err := permission.New(cfg.Permissions)
			if err != nil {
				t.Fatal(err)
			}
			defer base.Close()
			ceiling, err := permission.New(*cfg.PermissionModes.Maximum)
			if err != nil {
				t.Fatal(err)
			}
			defer ceiling.Close()
			controller := permission.NewController(t.Context(), base, ceiling, 0)
			defer controller.Close()
			if controller.Snapshot().Mode != permission.ReadOnly || base.CheckProcess() == nil || ceiling.CheckProcess() != nil {
				t.Fatal("daily config granted human elevation or lost its reviewed ceiling")
			}
			if sandbox == "asp" && cfg.Runtime.Provider.Auth.Method != "docker_sandbox_oauth" {
				t.Fatal("lost Docker OAuth contract")
			}
			if sandbox == "devlore" && (cfg.Runtime.ClaudeCode == nil || cfg.Runtime.ClaudeCode.ManagedPolicyMode != "trust" || cfg.Runtime.ClaudeCode.ToolBridge.ResolvedMode() != "structured") {
				t.Fatal("lost Team Structured Bridge contract")
			}
		})
	}
}

func dailyDevelopmentConfiguration(t *testing.T, sandbox, workspace, binary string) serveConfiguration {
	t.Helper()
	scripts, err := filepath.Abs(filepath.Join("..", "..", "scripts"))
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), "python3", "-B", "-c", `import json,sys
from pathlib import Path
sys.path.insert(0,sys.argv[1])
import daily_development as d
s=sys.argv[2]
print(json.dumps(d.configuration(s,Path(sys.argv[3]),"explicit-model","high",sys.argv[4],audited=s=="devlore",trust=s=="devlore")))`, scripts, sandbox, workspace, binary)
	data, err := command.Output()
	if err != nil {
		t.Fatal("generate daily configuration", err)
	}
	path := filepath.Join(t.TempDir(), "runtime.json")
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := readServeConfiguration(path)
	if err != nil || cfg.Runtime.Workspace != workspace || cfg.PermissionModes == nil || cfg.PermissionModes.Validate() != nil {
		t.Fatal("daily/native config mismatch", err)
	}
	return cfg
}

func TestDailyDevelopmentCreateEditReadProcessAndWorkspaceBoundary(t *testing.T) {
	checkWorkspaceToolReceipts(t, func(t *testing.T, cfg serveConfiguration) serveConfiguration {
		sandbox, binary := "asp", ""
		if cfg.Runtime.Provider.Provider == "claude-code" {
			sandbox, binary = "devlore", cfg.Runtime.ClaudeCode.Binary
		}
		daily := dailyDevelopmentConfiguration(t, sandbox, cfg.Runtime.Workspace, binary)
		// Only the synthetic Codex transport/auth reference is substituted. The
		// actual daily workspace, prompt, base policy and human ceiling are used.
		if sandbox == "asp" {
			daily.Runtime.Provider = cfg.Runtime.Provider
			daily.Permissions.NetworkOrigins = cfg.Permissions.NetworkOrigins
		}
		return daily
	})
}

//go:build linux || darwin

package main

import (
	"encoding/json/v2"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/permission"
)

func TestHumanPermissionAcceptanceKitUsesActualConfigurationSchema(t *testing.T) {
	scripts, err := filepath.Abs(filepath.Join("..", "..", "scripts"))
	if err != nil {
		t.Fatal(err)
	}
	for _, sandbox := range []string{"asp", "devlore"} {
		cmd := exec.CommandContext(t.Context(), "python3", "-B", "-c", `import json,sys
from pathlib import Path
sys.path.insert(0,sys.argv[1])
import permission_acceptance as gate
s=sys.argv[2]
print(json.dumps(gate.configuration(s,Path(sys.argv[3]),"explicit-model","high","/unused/claude",audited=s=="devlore",trust=s=="devlore")))`, scripts, sandbox, t.TempDir())
		data, err := cmd.Output()
		if err != nil {
			t.Fatal("generate actual kit configuration", err)
		}
		path := filepath.Join(t.TempDir(), "runtime.json")
		if err = os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := readServeConfiguration(path)
		if err != nil {
			t.Fatal("kit/Host schema mismatch", err)
		}
		if cfg.PermissionModes == nil || !cfg.PermissionModes.Enabled || cfg.PermissionModes.Validate() != nil {
			t.Fatal("mode opt-in missing")
		}
		base, err := permission.New(cfg.Permissions)
		if err != nil {
			t.Fatal(err)
		}
		defer base.Close()
		max, err := permission.New(*cfg.PermissionModes.Maximum)
		if err != nil {
			t.Fatal(err)
		}
		defer max.Close()
		c := permission.NewController(t.Context(), base, max, 0)
		defer c.Close()
		if c.Snapshot().Mode != permission.ReadOnly || base.CheckProcess() == nil || max.CheckProcess() != nil {
			t.Fatal("configuration granted approval or lost reviewed ambient ceiling")
		}
		if sandbox == "devlore" && (cfg.Runtime.ClaudeCode == nil || cfg.Runtime.ClaudeCode.ToolBridge.ResolvedMode() != "structured" || cfg.Runtime.ClaudeCode.ManagedPolicyMode != "trust") {
			t.Fatal("kit changed Claude contract")
		}
		if sandbox == "asp" && cfg.Runtime.Provider.Auth.Method != "docker_sandbox_oauth" {
			t.Fatal("kit lost external Docker OAuth reference")
		}
		var object map[string]any
		if err = json.Unmarshal(data, &object); err != nil {
			t.Fatal(err)
		}
		object["PermissionModes"].(map[string]any)["Mode"] = "full_access"
		bad, _ := json.Marshal(object)
		if err = os.WriteFile(path, bad, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err = readServeConfiguration(path); err == nil {
			t.Fatal("a config string became human approval")
		}
	}
}

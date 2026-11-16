package testcodex

import (
	"encoding/json/v2"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

func TestCatalogFixtureRecordsEnvironmentVerbatim(t *testing.T) {
	home := t.TempDir()
	binary := Install(t, home, nil)
	args := []string{"app-server", "--listen", "stdio://"}
	// Even unexpected variables must be recorded: filtering here could hide
	// leaks in the production adapter's subprocess environment.
	wantEnv := map[string]string{"HOME": home, "SDKROOT": "fixture-sdk", "UNREAL_FIXTURE_UNEXPECTED": "fixture-value"}
	cmd := exec.CommandContext(t.Context(), binary, args...)
	cmd.Dir = home
	for key, value := range wantEnv {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture failed: %v: %s", err, out)
	}
	data, err := os.ReadFile(filepath.Join(home, "codex-starts.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var record struct {
		Argv []string          `json:"argv"`
		Env  map[string]string `json:"env"`
		Cwd  string            `json:"cwd"`
	}
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	wantCwd, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(record.Argv, args) || !reflect.DeepEqual(record.Env, wantEnv) || record.Cwd != wantCwd {
		t.Fatalf("fixture changed subprocess inputs: %+v", record)
	}
}

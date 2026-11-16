// Package testcodex provides a metadata-only fake CLI. It has no inference path.
package testcodex

import (
	"encoding/json/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

var buildOnce sync.Once
var executable []byte
var buildErr error

// Install creates only public, synthetic protocol values in a private test HOME.
func Install(t *testing.T, home string, frames []any) string {
	t.Helper()
	// A native fixture records the environment supplied by the caller without
	// interpreter launchers adding SDKROOT or other platform-specific values.
	buildOnce.Do(func() {
		_, source, _, _ := runtime.Caller(0)
		dir, err := os.MkdirTemp("", "unreal-fake-codex-build-")
		if err != nil {
			buildErr = err
			return
		}
		defer os.RemoveAll(dir)
		path := filepath.Join(dir, "codex")
		cmd := exec.Command("go", "build", "-o", path, ".")
		cmd.Dir = filepath.Join(filepath.Dir(source), "cmd")
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = err
			t.Log(string(out))
			return
		}
		executable, buildErr = os.ReadFile(path)
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	data, err := json.Marshal(frames)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(home, "codex-frames.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "codex")
	if err = os.WriteFile(path, executable, 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

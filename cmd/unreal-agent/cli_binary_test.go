//go:build linux || darwin

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

// Executables are immutable and shared for this test process. Keep the race
// and ordinary builds separate, preserving each caller's instrumentation.
var cliBuilds [2]struct {
	once sync.Once
	path string
	err  error
	log  []byte
}

func TestMain(m *testing.M) {
	code := m.Run()
	for index := range cliBuilds {
		build := &cliBuilds[index]
		if build.path != "" {
			_ = os.RemoveAll(filepath.Dir(build.path))
		}
	}
	os.Exit(code)
}

func testCLI(t *testing.T, race bool) string {
	t.Helper()
	index := 0
	if race {
		index = 1
	}
	build := &cliBuilds[index]
	build.once.Do(func() {
		var directory string
		directory, build.err = os.MkdirTemp("", "ua-test-cli-")
		if build.err != nil {
			return
		}
		build.path = filepath.Join(directory, "unreal-agent")
		args := []string{"build", "-p", "1"}
		if race {
			args = append(args, "-race")
		}
		args = append(args, "-o", build.path, ".")
		build.log, build.err = exec.CommandContext(t.Context(), "go", args...).CombinedOutput()
	})
	if build.err != nil {
		t.Fatalf("build CLI: %v\n%s", build.err, build.log)
	}
	return build.path
}

// Only the subprocess receives these values; parallel tests never change the
// test process's environment or consult another test's HOME/Codex source.
func testCLIEnvironment(t *testing.T) []string {
	t.Helper()
	home := t.TempDir()
	return append(os.Environ(), "HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"XDG_STATE_HOME="+filepath.Join(home, ".local", "state"), "XDG_RUNTIME_DIR=",
		"CODEX_HOME="+filepath.Join(home, ".codex"), "OPENAI_CODEX_AUTH_FILE=")
}

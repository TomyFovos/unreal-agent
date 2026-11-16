//go:build linux || darwin

package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestLauncherSharedStateDirectory(t *testing.T) {
	root := privateCLIDirectory(t)
	env := map[string]string{"HOME": root}
	getenv := func(key string) string { return env[key] }
	state := filepath.Join(root, "state")
	shared := filepath.Join(root, "shared")

	// Without the flag the login and the /model-sub choice stay in STATE.
	o, err := parseLaunchOptions([]string{"name", "--state-directory", state}, io.Discard, getenv)
	if err != nil || o.defaultCredentials != filepath.Join(state, "credentials") || o.subagentModel != filepath.Join(state, "subagent-model.json") {
		t.Fatal(o, err)
	}

	// The flag moves both into the shared directory; state stays per folder.
	withFlag, err := parseLaunchOptions([]string{"name", "--state-directory", state, "--shared-state-directory", shared}, io.Discard, getenv)
	if err != nil || withFlag.defaultCredentials != filepath.Join(shared, "credentials") || withFlag.subagentModel != filepath.Join(shared, "subagent-model.json") || withFlag.state != state {
		t.Fatal(withFlag, err)
	}
	// The credentials stay lookup-only: --credential-directory is not set.
	if withFlag.credentials != "" || withFlag.config != o.config || withFlag.sessions != o.sessions || withFlag.socket != o.socket || withFlag.codexFile != o.codexFile || withFlag.normal != o.normal {
		t.Fatalf("shared state changed the Host binding: %+v vs %+v", withFlag, o)
	}

	// A relative shared directory becomes absolute.
	t.Chdir(root)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relative, err := parseLaunchOptions([]string{"name", "--state-directory", state, "--shared-state-directory", "shared"}, io.Discard, getenv)
	if err != nil || relative.defaultCredentials != filepath.Join(cwd, "shared", "credentials") || relative.subagentModel != filepath.Join(cwd, "shared", "subagent-model.json") {
		t.Fatal(relative, err)
	}
}

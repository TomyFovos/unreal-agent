//go:build linux || darwin

package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestLauncherSharedStateDirectory(t *testing.T) {
	root := privateCLIDirectory(t)
	env := map[string]string{"HOME": root}
	getenv := func(key string) string { return env[key] }
	state := filepath.Join(root, "state")
	shared := filepath.Join(root, "shared")

	// Without the flag the login and the subagent model choice stay in STATE.
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

func TestLauncherMigratesPerRepoSubagentModel(t *testing.T) {
	for _, choice := range []string{
		`{"Version":1,"Provider":"opencode-go","Model":"deepseek-v4.1-flash"}`,
		`{"Version":2,"Roles":{"worker":{"Provider":"opencode-go","Model":"deepseek-v4.1-flash","Effort":"max"},"collector":{"Provider":"openai-codex","Model":"gpt-6-luna","Effort":"medium"}}}`,
	} {
		t.Run(choice[:12], func(t *testing.T) {
			root := privateCLIDirectory(t)
			legacy := filepath.Join(root, "subagent-model.json")
			shared := filepath.Join(root, "shared", "subagent-model.json")
			if err := os.WriteFile(legacy, []byte(choice), 0600); err != nil {
				t.Fatal(err)
			}
			if err := migrateSubagentModel(legacy, shared); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{legacy, shared} {
				data, err := os.ReadFile(path)
				if err != nil || string(data) != choice {
					t.Fatalf("choice lost at %s: %v", path, err)
				}
			}
			if _, err := readSubagentModel(shared); err != nil {
				t.Fatal(err)
			}
			if info, err := os.Stat(shared); err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("shared model permissions", err)
			}
			// A later shared choice wins, including on subsequent launches of
			// the original repo. The legacy file is retained unchanged.
			updated := []byte(`{"Version":1,"Provider":"openai-codex"}`)
			if err := os.WriteFile(shared, updated, 0600); err != nil {
				t.Fatal(err)
			}
			if err := migrateSubagentModel(legacy, shared); err != nil {
				t.Fatal(err)
			}
			if data, err := os.ReadFile(shared); err != nil || !bytes.Equal(data, updated) {
				t.Fatal("existing shared choice overwritten", err)
			}
			if data, err := os.ReadFile(legacy); err != nil || string(data) != choice {
				t.Fatal("legacy choice changed", err)
			}
		})
	}
}

func TestLauncherSubagentModelMigrationConcurrentAndMissing(t *testing.T) {
	root := privateCLIDirectory(t)
	shared := filepath.Join(root, "shared", "subagent-model.json")
	if err := migrateSubagentModel(filepath.Join(root, "missing.json"), shared); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(shared); !os.IsNotExist(err) {
		t.Fatal("missing legacy choice created a shared choice", err)
	}
	choices := []string{`{"Version":1,"Provider":"openai-codex"}`, `{"Version":1,"Provider":"opencode-go","Model":"deepseek-v4.1-flash"}`}
	var group sync.WaitGroup
	for i, choice := range choices {
		legacy := filepath.Join(root, string(rune('a'+i))+".json")
		if err := os.WriteFile(legacy, []byte(choice), 0600); err != nil {
			t.Fatal(err)
		}
		group.Go(func() {
			if err := migrateSubagentModel(legacy, shared); err != nil {
				t.Error(err)
			}
		})
	}
	group.Wait()
	data, err := os.ReadFile(shared)
	if err != nil || string(data) != choices[0] && string(data) != choices[1] {
		t.Fatal("concurrent migration lost both choices", err)
	}
}

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestClaudeSubagentsUsesStableLauncherAcrossUpdates(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stable := filepath.Join(home, ".local", "bin", "unreal-agent")
	if err := os.MkdirAll(filepath.Dir(stable), 0700); err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"old", "new"} {
		if err := os.WriteFile(stable, []byte("#!/bin/sh\nexec /opt/"+version+"/unreal-agent \"$@\"\n"), 0700); err != nil {
			t.Fatal(err)
		}
		binary := filepath.Join(home, ".local", "opt", "unreal-agent-pilot", "common", version, "unreal")
		if got := subagentCLIPath(binary, home); got != stable {
			t.Fatalf("version=%s path=%s", version, got)
		}
	}
	source := filepath.Join(home, "src", "unreal-agent")
	if got := subagentCLIPath(source, home); got != source {
		t.Fatalf("source build picked another installation: %s", got)
	}
}

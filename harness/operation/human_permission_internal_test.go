package operation

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestHumanPermissionRequestShowsShellCommand(t *testing.T) {
	spec, err := NewShellSpec(ShellInput{Shell: "/bin/sh", Directory: t.TempDir(), Command: "curl -sS\n  https://example.test"}, t.TempDir(), 256)
	if err != nil {
		t.Fatal(err)
	}
	r := humanPermissionRequest(Operation{ID: "op", Type: spec.Type, Version: spec.Version, MaxOutputLength: spec.MaxOutputLength, State: spec.State})
	if r.Tool != "Bash" || !r.Privileged || r.Scope != "$ curl -sS https://example.test" {
		t.Fatalf("request = %+v", r)
	}
	long := shellScope(strings.Repeat("é", 400))
	if len(long) > 512 || !utf8.ValidString(long) || !strings.HasSuffix(long, "…") {
		t.Fatalf("long command scope = %d bytes, valid %v", len(long), utf8.ValidString(long))
	}
}

// Orchestration needs no approval: the child follows the mode and asks its
// parent (D1, U17).
func TestHumanPermissionRequestTreatsSubagentsAsOrchestration(t *testing.T) {
	for _, action := range []string{"start", "approve", "send", "finish", "parent"} {
		spec, err := NewRemoteJobSpec(RemoteJobPlan{Type: "subagent", Version: 1, Data: []byte(`{"Action":"` + action + `","Template":"lead"}`)})
		if err != nil {
			t.Fatal(err)
		}
		r := humanPermissionRequest(Operation{ID: "op", Type: spec.Type, Version: spec.Version, MaxOutputLength: DefaultMaxOutputLength, State: spec.State, ToolName: "SubagentStart"})
		if r.Privileged || !r.Orchestration || !strings.HasPrefix(r.Scope, "subagent "+action) {
			t.Fatalf("%s: %+v", action, r)
		}
	}
}

func TestHumanPermissionRequestNamesTheFile(t *testing.T) {
	data := []byte(`{"version":1,"action":"write","path":"/w/hello.txt","content":"hello"}`)
	spec, err := NewRemoteJobSpec(RemoteJobPlan{Type: "native_file", Version: 1, Data: data})
	if err != nil {
		t.Fatal(err)
	}
	r := humanPermissionRequest(Operation{ID: "op", Type: spec.Type, Version: spec.Version, MaxOutputLength: DefaultMaxOutputLength, State: spec.State, ToolName: "write"})
	if r.Tool != "write" || !r.Privileged || r.Scope != "write /w/hello.txt" {
		t.Fatalf("%+v", r)
	}
}

// A command that only reads runs in every mode, as Codex runs its known-safe
// commands.
func TestHumanPermissionRequestLetsReadOnlyCommandsRun(t *testing.T) {
	for command, readOnly := range map[string]bool{"git log --oneline -5": true, "git status && git show HEAD": true, "go test ./...": false, "git commit -m x": false} {
		spec, err := NewShellSpec(ShellInput{Shell: "/bin/sh", Directory: t.TempDir(), Command: command}, t.TempDir(), 256)
		if err != nil {
			t.Fatal(err)
		}
		r := humanPermissionRequest(Operation{ID: "op", Type: spec.Type, Version: spec.Version, MaxOutputLength: spec.MaxOutputLength, State: spec.State})
		if r.ReadOnlyCommand != readOnly || r.Privileged == readOnly || r.Scope != "$ "+command {
			t.Fatalf("%q: %+v", command, r)
		}
	}
}

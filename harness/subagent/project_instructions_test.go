package subagent_test

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/projectinstructions"
	"github.com/unreallabsai/unreal-agent/harness/session"
	sub "github.com/unreallabsai/unreal-agent/harness/subagent"
)

func instructionParent(t *testing.T, separateWorkspace bool) (*host.Host, *host.Session, sub.Config, string) {
	t.Helper()
	directory, workspace := t.TempDir(), t.TempDir()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	config := sub.Config{Directory: directory, Binary: binary, Arguments: []string{"-test.run=^TestChildServerHelper$"}, Environment: append(os.Environ(), "UNREAL_SUBAGENT_HELPER=1"),
		Templates: map[string]sub.Template{"worker": {Workspace: workspace, Runtime: []byte(`{}`), Policy: permission.Config{Tools: []string{"Finish", "SendParent"}}}}}
	parentWorkspace := workspace
	if separateWorkspace {
		parentWorkspace = t.TempDir()
		writeInstructionFile(t, workspace, "Child revision A.")
	}
	writeInstructionFile(t, parentWorkspace, "Parent revision A.")
	h, err := host.New(t.Context(), host.Config{Directory: directory, Build: instructionFactory(config)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	parent, err := h.Create(t.Context(), host.Options{ID: "parent", Workspace: parentWorkspace, Policy: permission.Unrestricted()})
	if err != nil {
		t.Fatal(err)
	}
	return h, parent, config, workspace
}

func instructionFactory(config sub.Config) host.Factory {
	return factory(config, adapter(func(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
		return llm.Response{}, nil
	}))
}

func writeInstructionFile(t *testing.T, workspace, content string) projectinstructions.Snapshot {
	t.Helper()
	if err := os.WriteFile(filepath.Join(workspace, projectinstructions.FileName), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := projectinstructions.FromContent([]byte(content))
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func awaitInstructionChild(t *testing.T, parent *host.Session, id operation.ID) operation.Operation {
	t.Helper()
	var child operation.Operation
	awaitBoundary(t, func() bool {
		view, err := parent.Inspect(0, 256)
		if err != nil {
			t.Fatal(err)
		}
		for _, op := range view.Operations {
			if op.ID == id && (op.Status == operation.StatusCompleted || op.Status == operation.StatusFailed || op.Status == operation.StatusCanceled) {
				child = op
				return true
			}
		}
		return false
	})
	return child
}

func TestChildProjectInstructionsDiscoveredAtStart(t *testing.T) {
	t.Run("shared workspace", func(t *testing.T) { testChildInstructionsAtStart(t, false) })
	t.Run("different workspace", func(t *testing.T) { testChildInstructionsAtStart(t, true) })
}

func testChildInstructionsAtStart(t *testing.T, separateWorkspace bool) {
	h, parent, config, workspace := instructionParent(t, separateWorkspace)
	parentSnapshot := parent.BoundProjectInstructions()
	if parentSnapshot == nil {
		t.Fatal("parent did not bind its original instructions")
	}
	start := func(input inbox.ID, want projectinstructions.Snapshot) sub.ChildConfig {
		t.Helper()
		if _, err := sub.StartChild(t.Context(), parent, parent.Generation, input, "worker", "finish", config.Templates, nil, nil); err != nil {
			t.Fatal(err)
		}
		op := awaitInstructionChild(t, parent, sub.StartOperationID(parent.ID, input))
		if op.Status != operation.StatusCompleted {
			t.Fatal("child start failed", op)
		}
		plan, err := sub.DecodePlan(op)
		if err != nil {
			t.Fatal(err)
		}
		child, err := sub.ReadChild(t.Context(), config.Directory, plan.ChildID, 0, 256)
		if err != nil || child.Configuration == nil || child.Configuration.ProjectInstructions == nil {
			t.Fatal("child did not record its instruction snapshot", child, err)
		}
		if got := *child.Configuration.ProjectInstructions; got != want {
			t.Fatalf("child snapshot = %+v; want %+v", got, want)
		}
		if child.View.ProjectInstructions == nil || *child.View.ProjectInstructions != want.Metadata() {
			t.Fatal("child instruction digest", child.View.ProjectInstructions)
		}
		return *child.Configuration
	}
	// AGENTS.md is edited after the parent Session was created. Every new
	// child binds its own workspace's current bytes, even in another workspace.
	snapshotB := writeInstructionFile(t, workspace, "Child revision B.")
	firstChild := start("start-b", snapshotB)
	firstConfiguration, err := json.Marshal(firstChild)
	if err != nil {
		t.Fatal(err)
	}
	snapshotC := writeInstructionFile(t, workspace, "Child revision C.")
	start("start-c", snapshotC)
	if err := os.Remove(filepath.Join(workspace, projectinstructions.FileName)); err != nil {
		t.Fatal(err)
	}
	start("start-none", projectinstructions.None())
	if got := parent.BoundProjectInstructions(); got == nil || *got != *parentSnapshot {
		t.Fatal("new child changed the parent's snapshot", got)
	}
	h.Close()
	// Even an unreadable-as-instructions file must not affect resume/restart
	// of the parent or an existing child: both replay their own saved snapshots.
	if err := os.WriteFile(filepath.Join(workspace, projectinstructions.FileName), []byte("bad\x00binary"), 0600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		restarted, err := host.New(t.Context(), host.Config{Directory: config.Directory, Build: instructionFactory(config)})
		if err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			id            session.ID
			lifecycle     string
			configuration jsontext.Value
			want          projectinstructions.Snapshot
		}{
			{parent.ID, "", nil, *parentSnapshot},
			{firstChild.ChildID, "child", firstConfiguration, snapshotB},
		} {
			resumed, err := restarted.Resume(t.Context(), host.Options{ID: tc.id, Lifecycle: tc.lifecycle, Configuration: tc.configuration, Workspace: workspace, Policy: permission.Unrestricted()})
			if err != nil {
				restarted.Close()
				t.Fatal("resume", err)
			}
			if got := resumed.BoundProjectInstructions(); got == nil || *got != tc.want {
				restarted.Close()
				t.Fatal("resume replaced the saved snapshot", got)
			}
		}
		restarted.Close()
	}
}

func TestChildProjectInstructionsDiscoveryFailure(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		code          projectinstructions.Code
	}{
		{"binary", "bad\x00binary", projectinstructions.CodeBinary},
		{"encoding", "\xff", projectinstructions.CodeInvalidEncoding},
		{"directory", "", projectinstructions.CodeUnreadable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, parent, config, workspace := instructionParent(t, false)
			path := filepath.Join(workspace, projectinstructions.FileName)
			var err error
			if tc.name == "directory" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				err = os.Mkdir(path, 0700)
			} else {
				err = os.WriteFile(path, []byte(tc.content), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = sub.StartChild(t.Context(), parent, parent.Generation, "start-broken", "worker", "finish", config.Templates, nil, nil); err != nil {
				t.Fatal(err)
			}
			op := awaitInstructionChild(t, parent, sub.StartOperationID(parent.ID, "start-broken"))
			state, err := operation.DecodeRemoteJobState(op)
			if err != nil || op.Status != operation.StatusFailed || !strings.Contains(state.TerminalError, "project instructions") || !strings.Contains(state.TerminalError, string(tc.code)) {
				t.Fatal("discovery failure was not reported", op.Status, state.TerminalError, err)
			}
			childID := sub.ChildID(parent.ID, op.ID)
			if _, err = os.Stat(filepath.Join(config.Directory, string(childID)+".session.jsonl")); !os.IsNotExist(err) {
				t.Fatal("unreadable instructions created a child using a stale snapshot", err)
			}
		})
	}
}

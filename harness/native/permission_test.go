//go:build linux || darwin

package native

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/mutation"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
)

func TestFilesystemDenialRemainsTypedThroughManager(t *testing.T) {
	root := t.TempDir()
	policy, err := permission.New(permission.Config{Tools: []string{"write"}})
	if err != nil {
		t.Fatal(err)
	}
	defer policy.Close()
	ctx := permission.WithPolicy(t.Context(), policy)
	files, err := mutation.New(mutation.Config{Root: root, StateDir: filepath.Join(t.TempDir(), "state"), Authorize: func(ctx context.Context, path string, write bool) error {
		return permission.FromContext(ctx).CheckPath(path, write)
	}})
	if err != nil {
		t.Fatal(err)
	}
	handler, _ := NewHandler(ctx, Executor{Files: files}, "session")
	manager := operation.NewLocalOperationManagerWithPolicy(ctx, policy, handler)
	spec, err := Spec(Request{Version: Version, Action: "write", Path: "a", Expected: &mutation.Revision{}, Content: "forbidden"})
	if err != nil {
		t.Fatal(err)
	}
	current := operation.Operation{ToolName: "write", ID: "denied", Type: spec.Type, Version: spec.Version, State: spec.State, MaxOutputLength: spec.MaxOutputLength, Status: operation.StatusReady}
	if err = manager.Add(current); err != nil {
		t.Fatal("denial terminated Add", err)
	}
	select {
	case got := <-manager.Updates():
		if got.Status != operation.StatusFailed || got.Denial == nil || got.Denial.Capability != "filesystem" {
			t.Fatal(got)
		}
	case <-time.After(time.Second):
		t.Fatal("denial blocked")
	}
}

//go:build linux || darwin

package native

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/mutation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
)

func TestReadsUseShellScopeInEveryMode(t *testing.T) {
	for _, mode := range []permission.Mode{permission.ReadOnly, permission.Ask, permission.FullAccess} {
		t.Run(string(mode), func(t *testing.T) {
			workspace, outside := t.TempDir(), t.TempDir()
			path := filepath.Join(outside, "example.txt")
			if err := os.WriteFile(path, []byte("outside needle\n"), 0600); err != nil {
				t.Fatal(err)
			}
			baseline, err := permission.New(permission.Config{Tools: []string{"read", "grep", "glob", "bash"}, ReadRoots: []string{workspace}})
			if err != nil {
				t.Fatal(err)
			}
			defer baseline.Close()
			controller := permission.NewController(t.Context(), baseline, permission.Unrestricted(), 0)
			defer controller.Close()
			if err := controller.Follow(mode); err != nil {
				t.Fatal(err)
			}
			ctx := permission.WithController(t.Context(), controller)
			files, err := mutation.New(mutation.Config{Root: workspace, StateDir: filepath.Join(t.TempDir(), "private-state"), Authorize: func(ctx context.Context, path string, write bool) error {
				return permission.FromContext(ctx).CheckPath(path, write)
			}})
			if err != nil {
				t.Fatal(err)
			}
			shell := <-controller.Request(ctx, permission.Request{ID: "shell", Tool: "bash", ReadOnlyCommand: true})
			if shell.Denial != nil || permission.FromContext(shell.Context).CheckPath(path, false) != nil {
				t.Fatal("shell cannot read test file")
			}
			shell.Release()
			for _, action := range []string{"read", "grep", "glob"} {
				permit := <-controller.Request(ctx, permission.Request{ID: action, Tool: action})
				if permit.Denial != nil {
					t.Fatal(permit.Denial)
				}
				r := Request{Version: 1, Action: action, Path: outside, Pattern: "needle"}
				if action == "read" {
					r.Path = path
				}
				got := (Executor{Files: files}).Execute(permit.Context, action, r)
				permit.Release()
				if got.Code != "ok" || action == "read" && got.Content != "outside needle\n" || action != "read" && len(got.Matches) != 1 {
					t.Fatalf("%s: %+v", action, got)
				}
			}
			if _, err := files.Path(path); mutation.FailureReason(err) != mutation.ReasonWorkspaceBoundary {
				t.Fatal("mutation workspace boundary removed", err)
			}
			// Private mutation receipts remain excluded even with ambient shell access.
			if got := (Executor{Files: files}).Execute(permission.WithPolicy(t.Context(), permission.Unrestricted()), "private", Request{Version: 1, Action: "read", Path: filepath.Join(files.StateDirectory(), "receipt")}); got.Reason != mutation.ReasonInternalState {
				t.Fatal(got)
			}
			for _, path := range []string{filepath.Join(outside, ".codex", "auth.json"), filepath.Join(outside, ".claude", ".credentials.json")} {
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("synthetic private material"), 0600); err != nil {
					t.Fatal(err)
				}
				permit := <-controller.Request(ctx, permission.Request{ID: "secret-" + filepath.Base(path), Tool: "read"})
				got := (Executor{Files: files}).Execute(permit.Context, "secret", Request{Version: 1, Action: "read", Path: path})
				permit.Release()
				if got.Code != "permission_denied" || got.Content != "" {
					t.Fatal("credential exclusion lost", got)
				}
			}
		})
	}
}

func TestOutsideReadsPreserveExplicitPolicyExclusions(t *testing.T) {
	workspace, allowed, secret := t.TempDir(), t.TempDir(), t.TempDir()
	policy, err := permission.New(permission.Config{ReadRoots: []string{workspace, allowed}})
	if err != nil {
		t.Fatal(err)
	}
	defer policy.Close()
	files, err := mutation.New(mutation.Config{Root: workspace, StateDir: t.TempDir(), Authorize: func(ctx context.Context, path string, write bool) error {
		return permission.FromContext(ctx).CheckPath(path, write)
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{allowed, secret} {
		if err := os.WriteFile(filepath.Join(dir, "data"), []byte("needle"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	ctx := permission.WithPolicy(t.Context(), policy)
	for _, action := range []string{"read", "grep", "glob"} {
		for _, dir := range []string{allowed, secret} {
			r := Request{Version: 1, Action: action, Path: dir, Pattern: "needle"}
			if action == "read" {
				r.Path = filepath.Join(dir, "data")
			}
			got := (Executor{Files: files}).Execute(ctx, "test", r)
			if dir == allowed && got.Code != "ok" || dir == secret && got.Code != "permission_denied" {
				t.Fatalf("%s: %+v", action, got)
			}
		}
	}
}

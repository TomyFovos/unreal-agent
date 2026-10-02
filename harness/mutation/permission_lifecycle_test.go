//go:build linux || darwin

package mutation

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
)

func TestPinnedPolicyRejectsRootReplacement(t *testing.T) {
	for _, nested := range []bool{false, true} {
		for _, action := range []string{"read", "write", "directory"} {
			t.Run(action+map[bool]string{true: "_nested", false: "_root"}[nested], func(t *testing.T) {
				workspace := filepath.Join(t.TempDir(), "workspace")
				os.MkdirAll(workspace, 0700)
				allowed := workspace
				if nested {
					allowed = filepath.Join(workspace, "allowed")
					os.MkdirAll(allowed, 0700)
				}
				path := filepath.Join(allowed, "a")
				os.WriteFile(path, []byte("original"), 0600)
				policy, err := permission.New(permission.Config{ReadRoots: []string{allowed}, WriteRoots: []string{allowed}})
				if err != nil {
					t.Fatal(err)
				}
				defer policy.Close()
				ctx := permission.WithPolicy(t.Context(), policy)
				var once sync.Once
				files, err := New(Config{Root: workspace, StateDir: filepath.Join(t.TempDir(), "state"), Authorize: func(ctx context.Context, path string, write bool) error {
					if err := permission.FromContext(ctx).CheckPath(path, write); err != nil {
						return err
					}
					once.Do(func() {
						if err := os.Rename(allowed, allowed+"-old"); err != nil {
							t.Fatal(err)
						}
						os.MkdirAll(allowed, 0700)
						os.WriteFile(filepath.Join(allowed, "a"), []byte("decoy"), 0600)
					})
					return nil
				}})
				if err != nil {
					t.Fatal(err)
				}
				switch action {
				case "read":
					_, err = files.Snapshot(ctx, path)
					if permission.Failure(err) == nil {
						t.Fatal("replaced read accepted", err)
					}
				case "directory":
					_, _, err = files.ReadDir(ctx, allowed, 100)
					if permission.Failure(err) == nil {
						t.Fatal("replaced directory accepted", err)
					}
				case "write":
					result := files.Apply(ctx, request(path, RevisionOf([]byte("original")), "changed"))
					if result.Code != Denied || result.Denial == nil {
						t.Fatal(result)
					}
				}
				current, _ := os.ReadFile(path)
				original, _ := os.ReadFile(filepath.Join(allowed+"-old", "a"))
				if string(current) != "decoy" || string(original) != "original" {
					t.Fatal(string(current), string(original))
				}
			})
		}
	}
}
func TestMutationHandlerCloseDrainsAndRejectsNewWork(t *testing.T) {
	started := make(chan struct{})
	exited := make(chan struct{})
	files, err := New(Config{Root: t.TempDir(), StateDir: filepath.Join(t.TempDir(), "state"), Authorize: func(ctx context.Context, _ string, _ bool) error {
		close(started)
		<-ctx.Done()
		close(exited)
		return ctx.Err()
	}})
	if err != nil {
		t.Fatal(err)
	}
	handler, _ := NewHandler(t.Context(), files, "session")
	spec, _ := Spec(request("a", Revision{}, "x"))
	current := operation.Operation{ID: "op", Type: spec.Type, Version: spec.Version, MaxOutputLength: spec.MaxOutputLength, State: spec.State, Status: operation.StatusReady}
	if err = handler.AddRemoteJob(current); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("worker not started")
	}
	if err = handler.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	default:
		t.Fatal("Close returned before worker")
	}
	handler.mu.Lock()
	active := len(handler.jobs)
	handler.mu.Unlock()
	if active != 0 {
		t.Fatal("completed jobs retained")
	}
	if _, open := <-handler.RemoteJobUpdates(); open {
		t.Fatal("updates not closed")
	}
	if handler.AddRemoteJob(current) == nil {
		t.Fatal("closed handler accepted")
	}
	if handler.Close() != nil {
		t.Fatal("second close")
	}
}

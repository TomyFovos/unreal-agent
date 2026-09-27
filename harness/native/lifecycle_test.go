//go:build linux || darwin

package native

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/mutation"
	"github.com/unreallabsai/unreal-agent/harness/operation"
)

func TestHandlerCloseDrainsWorkers(t *testing.T) {
	started := make(chan struct{})
	exited := make(chan struct{})
	files, err := mutation.New(mutation.Config{Root: t.TempDir(), StateDir: filepath.Join(t.TempDir(), "state"), Authorize: func(ctx context.Context, _ string, _ bool) error {
		close(started)
		<-ctx.Done()
		close(exited)
		return ctx.Err()
	}})
	if err != nil {
		t.Fatal(err)
	}
	handler, _ := NewHandler(t.Context(), Executor{Files: files}, "session")
	spec, _ := Spec(Request{Version: Version, Action: "read", Path: "a"})
	current := operation.Operation{ID: "op", Type: spec.Type, Version: spec.Version, State: spec.State, MaxOutputLength: spec.MaxOutputLength, Status: operation.StatusReady}
	if err = handler.AddRemoteJob(current); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("not started")
	}
	var closers sync.WaitGroup
	for range 3 {
		closers.Go(func() { handler.Close() })
	}
	closers.Wait()
	select {
	case <-exited:
	default:
		t.Fatal("Close did not drain")
	}
	handler.mu.Lock()
	active := len(handler.jobs)
	handler.mu.Unlock()
	if active != 0 {
		t.Fatal("active jobs retained")
	}
	if _, open := <-handler.RemoteJobUpdates(); open {
		t.Fatal("updates still open")
	}
	if handler.AddRemoteJob(current) == nil {
		t.Fatal("closed handler accepted")
	}
}
func TestConcurrentAddAndClose(t *testing.T) {
	for range 30 {
		executor, _ := executor(t)
		handler, _ := NewHandler(t.Context(), executor, "session")
		spec, _ := Spec(Request{Version: Version, Action: "read", Path: "a"})
		current := operation.Operation{ID: "op", Type: spec.Type, Version: spec.Version, State: spec.State, MaxOutputLength: spec.MaxOutputLength, Status: operation.StatusReady}
		var workers sync.WaitGroup
		workers.Go(func() { handler.AddRemoteJob(current) })
		workers.Go(func() { handler.Close() })
		workers.Wait()
		handler.mu.Lock()
		active := len(handler.jobs)
		handler.mu.Unlock()
		if active != 0 {
			t.Fatal("Close missed Add")
		}
	}
}

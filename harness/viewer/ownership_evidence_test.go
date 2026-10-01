package viewer

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/host/gateway"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	"github.com/unreallabsai/unreal-agent/harness/subagent"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

type viewerOwnershipFixture struct {
	host          *host.Host
	parent, child *host.Session
	dir           string
	builds        *atomic.Int32
}

func ownershipFixture(t *testing.T) viewerOwnershipFixture {
	t.Helper()
	dir := t.TempDir()
	builds := &atomic.Int32{}
	h, err := host.New(t.Context(), host.Config{Directory: dir, Build: func(context.Context, session.ID) (host.Runtime, error) {
		builds.Add(1)
		return host.Runtime{Builder: contextbuilder.NewBuilder(), LLM: &viewerLLM{}, Tools: tool.NewRegistry(tool.StaticTranslators{}), Operations: newHeldManager()}, nil
	}})
	must(t, err)
	t.Cleanup(func() { h.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	parent, err := h.Create(ctx, host.Options{ID: "parent", Policy: permission.Unrestricted()})
	must(t, err)
	_, err = parent.SubmitOperation(ctx, parent.Generation, "start-request", "start-op", "SubagentStart", startSpec(t, parent.ID, dir))
	must(t, err)
	id := subagent.ChildID(parent.ID, "start-op")
	config := subagent.ChildConfig{Version: 1, ParentID: parent.ID, ChildID: id, OperationID: "start-op", SessionDirectory: dir, Workspace: dir, Runtime: jsontext.Value("{}"), ReadyID: "ready:start-op", Task: "inspect task", Policy: permission.Config{Tools: []string{"Finish"}}}
	raw, err := json.Marshal(config)
	must(t, err)
	child, err := h.Create(ctx, host.Options{ID: id, Lifecycle: "child", Configuration: raw, Policy: permission.Unrestricted()})
	must(t, err)
	return viewerOwnershipFixture{h, parent, child, dir, builds}
}

func fixtureLog(t *testing.T, f viewerOwnershipFixture, id session.ID) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.dir, string(id)+".session.jsonl"))
	must(t, err)
	return data
}

func TestObservationReconnectAndRefreshNeverResumeStoppedChild(t *testing.T) {
	f := ownershipFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err := f.child.Stop(ctx, f.child.Generation, inbox.StopHard, "stopped fixture")
	must(t, err)
	must(t, f.child.Wait(ctx))
	beforeParent, beforeChild := fixtureLog(t, f, f.parent.ID), fixtureLog(t, f, f.child.ID)
	store, err := localfile.New(f.dir)
	must(t, err)
	lock, err := store.AcquireWriter(f.child.ID)
	must(t, err)
	defer lock.Close()
	// A disconnected frontend is replaced by a new frontend while a separate
	// writer gate is held. Observation must neither open nor acquire the child.
	for range 2 {
		reader := ParentReader{Host: f.host, ParentID: f.parent.ID, Directory: f.dir}
		c := NewClient(reader, New(SubagentOptions()), HostControls{Host: f.host})
		must(t, c.Refresh(ctx, f.parent.ID))
		must(t, c.Refresh(ctx, f.child.ID))
		must(t, c.Watch(ctx, f.child.ID, nil))
		_, err = c.History(ctx, f.child.ID, 0, 1)
		must(t, err)
		observed := row(t, c.Model, f.child.ID)
		if observed.Runtime != RuntimeUnknown || observed.Generation != "" {
			t.Fatal("persisted stopped child invented runtime ownership", observed)
		}
		select {
		case <-f.child.Done():
		default:
			t.Fatal("observation resurrected child")
		}
	}
	if f.builds.Load() != 2 || !bytes.Equal(beforeParent, fixtureLog(t, f, f.parent.ID)) || !bytes.Equal(beforeChild, fixtureLog(t, f, f.child.ID)) {
		t.Fatal("refresh/reconnect acquired a runtime or mutated canonical state")
	}
	// A user request with a stopped parent remains unavailable; only explicit
	// Host.Open/Resume can pass the ownership/recovery gate for that parent.
	_, err = f.parent.Stop(ctx, f.parent.Generation, inbox.StopHard, "stop parent")
	must(t, err)
	must(t, f.parent.Wait(ctx))
	req := ControlRequest{ParentID: f.parent.ID, ParentGeneration: f.parent.Generation, OperationID: "start-op", ChildID: f.child.ID, InputID: "explicit-resume"}
	if err = (HostControls{Host: f.host}).ResumeChild(ctx, req); err == nil {
		t.Fatal("stopped parent automatically reopened")
	}
	if f.builds.Load() != 2 {
		t.Fatal("viewer reopened a stopped parent")
	}
}

func TestConcurrentViewerResumeViaGatewayKeepsSingleHostWriter(t *testing.T) {
	f := ownershipFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	// Use a short private socket path on both Linux and macOS.
	socketDir, err := os.MkdirTemp("/tmp", "ua-viewer-")
	must(t, err)
	defer os.RemoveAll(socketDir)
	socket := filepath.Join(socketDir, "host.sock")
	serverCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- gateway.ListenAndServe(serverCtx, socket, gateway.Config{Host: f.host, Extension: ExtensionHandler{Host: f.host, Directory: f.dir}})
	}()
	defer func() { stop(); must(t, <-done) }()
	probe := gateway.NewClient(socket)
	defer probe.Close()
	for {
		if _, err = probe.Inspect(ctx, f.parent.ID, 0, 1); err == nil {
			break
		}
		if !pause(ctx, 5*time.Millisecond) {
			t.Fatal("viewer gateway startup deadline")
		}
	}
	beforeParent, beforeChild := fixtureLog(t, f, f.parent.ID), fixtureLog(t, f, f.child.ID)
	const clients = 8
	ready := make(chan struct{}, clients)
	gate := make(chan struct{})
	results := make(chan error, clients)
	for range clients {
		go func() {
			local := gateway.NewClient(socket)
			defer local.Close()
			remote := Remote{ParentID: f.parent.ID, Parent: local, Call: local.Extension}
			c := NewClient(remote, New(SubagentOptions()), remote)
			if err := c.Refresh(ctx, f.parent.ID); err != nil {
				ready <- struct{}{}
				results <- err
				return
			}
			ready <- struct{}{}
			select {
			case <-gate:
			case <-ctx.Done():
				results <- context.Cause(ctx)
				return
			}
			results <- c.Resume(ctx, f.child.ID, "duplicate-resume")
		}()
	}
	for range clients {
		select {
		case <-ready:
		case <-ctx.Done():
			t.Fatal("concurrent viewer setup deadline")
		}
	}
	close(gate)
	for range clients {
		select {
		case err := <-results:
			must(t, err)
		case <-ctx.Done():
			t.Fatal("concurrent viewer resume deadline")
		}
	}
	current, err := f.host.Attach(f.child.ID)
	must(t, err)
	if current != f.child || current.Generation != f.child.Generation || f.builds.Load() != 2 {
		t.Fatal("duplicate resume opened another runtime")
	}
	store, err := localfile.New(f.dir)
	must(t, err)
	if lock, err := store.AcquireWriter(f.child.ID); !errors.Is(err, localfile.ErrWriterOwned) {
		if lock != nil {
			lock.Close()
		}
		t.Fatal("actual child writer ownership changed", err)
	}
	if !bytes.Equal(beforeParent, fixtureLog(t, f, f.parent.ID)) || !bytes.Equal(beforeChild, fixtureLog(t, f, f.child.ID)) {
		t.Fatal("duplicate resume directly mutated canonical history")
	}
}

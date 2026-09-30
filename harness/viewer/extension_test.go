package viewer

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/host/gateway"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/subagent"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSocket(t *testing.T) {
	dir := t.TempDir()
	must(t, os.Chmod(dir, 0700))
	h := viewerOwner(t, dir)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	parent, err := h.Create(ctx, host.Options{ID: "parent", Lifecycle: "interactive", Policy: permission.Unrestricted()})
	must(t, err)
	_, err = parent.SubmitOperation(ctx, parent.Generation, "start-request", "start-op", "SubagentStart", startSpec(t, parent.ID, dir))
	must(t, err)
	id := subagent.ChildID(parent.ID, "start-op")
	config := subagent.ChildConfig{Version: 1, ParentID: parent.ID, ChildID: id, OperationID: "start-op", SessionDirectory: dir, Workspace: dir, Runtime: jsontext.Value("{}"), ReadyID: "ready:start-op", Task: "inspect task", Policy: permission.Config{Tools: []string{"Finish"}}}
	encoded, _ := json.Marshal(config)
	child, err := h.Create(ctx, host.Options{ID: id, Lifecycle: "child", Configuration: encoded, Policy: permission.Unrestricted()})
	must(t, err)
	socket := filepath.Join(dir, "host.sock")
	serverCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- gateway.ListenAndServe(serverCtx, socket, gateway.Config{Host: h, Extension: ExtensionHandler{Host: h, Directory: dir}})
	}()
	defer func() { stop(); must(t, <-done) }()
	local := gateway.NewClient(socket)
	defer local.Close()
	for {
		if _, err = local.Inspect(ctx, parent.ID, 0, 1); err == nil {
			break
		}
		if !pause(ctx, 10*time.Millisecond) {
			t.Fatal("gateway never became ready")
		}
	}
	remote := Remote{ParentID: parent.ID, Parent: local, Call: local.Extension}
	c := NewClient(remote, New(SubagentOptions()), remote)
	must(t, c.Refresh(ctx, parent.ID))
	must(t, c.Refresh(ctx, id))
	observed := row(t, c.Model, id)
	if observed.Runtime != RuntimeUnknown || observed.Generation != "" {
		t.Fatal("offline snapshot claimed liveness")
	}
	receipt, err := c.Steer(ctx, id, "socket-steer", "please continue")
	must(t, err)
	same, err := c.Steer(ctx, id, "socket-steer", "please continue")
	must(t, err)
	if receipt != same || receipt.Sequence == 0 {
		t.Fatal("wire lost committed receipt")
	}
	must(t, c.Resume(ctx, id, "socket-resume"))
	current, err := h.Attach(id)
	must(t, err)
	if current.Generation != child.Generation {
		t.Fatal("socket request opened another writer")
	}
	if _, err = remote.Inspect(ctx, "arbitrary-session", 0, 1); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	bad := ControlRequest{ParentID: parent.ID, ParentGeneration: "old", ChildID: id, OperationID: "start-op", InputID: "stale"}
	if _, err = remote.CancelChild(ctx, bad); !errors.Is(err, host.ErrStaleGeneration) {
		t.Fatal(err)
	}
	_, err = c.Cancel(ctx, id, "socket-cancel", "stop")
	must(t, err)
	// Closing this observer's transport only must not close the Host.
	local.Close()
	select {
	case <-parent.Done():
		t.Fatal("detach stopped owner")
	default:
	}
}
func TestExtensionBoundaryAndMalformedReplies(t *testing.T) {
	handler := ExtensionHandler{}
	for _, body := range []string{"{", `{"Action":"inspect","Unknown":true}`, strings.Repeat("x", maxExtensionRequest+1)} {
		req := httptest.NewRequest(http.MethodPost, "http://localhost/v1/extension", strings.NewReader(body))
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, req)
		if !strings.Contains(out.Body.String(), "invalid_request") {
			t.Fatal(out.Body.String())
		}
	}
	req := httptest.NewRequest(http.MethodPost, "http://localhost/v1/extension", bytes.NewReader(nil))
	req.Header.Set("Origin", "https://external.example")
	out := httptest.NewRecorder()
	handler.ServeHTTP(out, req)
	if out.Code != http.StatusBadRequest {
		t.Fatal("cross-origin request accepted")
	}
	remote := Remote{ParentID: "parent", Call: func(context.Context, jsontext.Value) (jsontext.Value, error) {
		return jsontext.Value(`{"Receipt":{"ID":"wrong","Sequence":1}}`), nil
	}}
	if _, err := remote.SteerChild(t.Context(), ControlRequest{ParentID: "parent", InputID: "correct"}); err == nil {
		t.Fatal("wrong receipt accepted")
	}
	remote.Call = func(context.Context, jsontext.Value) (jsontext.Value, error) {
		return jsontext.Value(`{"View":{"Session":{"Session":{"ID":"wrong"}}}}`), nil
	}
	if _, err := remote.Inspect(t.Context(), "child", 0, 1); err == nil {
		t.Fatal("wrong child snapshot accepted")
	}
	if _, err := remote.SteerChild(t.Context(), ControlRequest{ParentID: "other", InputID: "request"}); !errors.Is(err, ErrUnavailable) {
		t.Fatal("cross-parent control forwarded")
	}
}

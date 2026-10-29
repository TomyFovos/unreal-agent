//go:build linux || darwin

package gateway

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHumanPermissionGatewayRequiresAuthenticatedPeerAndBoundLease(t *testing.T) {
	dir, err := os.MkdirTemp("", "ua-mode-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	owner, err := host.New(t.Context(), host.Config{Directory: filepath.Join(dir, "sessions"), PermissionMaximum: permission.Unrestricted(), Build: func(ctx context.Context, id session.ID) (host.Runtime, error) {
		return host.Runtime{Builder: contextbuilder.NewBuilder(), LLM: model{}, Tools: tool.NewRegistry(tool.StaticTranslators{}), Operations: operation.NewLocalOperationManager(ctx)}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	socket := filepath.Join(dir, "host.sock")
	go func() { done <- ListenAndServe(ctx, socket, Config{Host: owner, Policy: permission.Unrestricted()}) }()
	client := NewClient(socket)
	defer client.Close()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := client.Methods(t.Context()); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("gateway not ready")
		}
		time.Sleep(time.Millisecond)
	}
	v, err := client.Open(t.Context(), host.Create, "human")
	if err != nil {
		t.Fatal(err)
	}
	w, err := client.WatchPermissions(t.Context(), "human", v.Generation)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Cancel()
	rev := w.Initial.Revision
	out, err := w.Request(t.Context(), permission.ControlRequest{Action: "set_mode", Expected: rev, Mode: permission.FullAccess})
	if err != nil || out.Snapshot.Mode != permission.FullAccess {
		t.Fatal(err, out)
	}
	if _, err = w.Request(t.Context(), permission.ControlRequest{Action: "set_mode", Expected: rev, Mode: permission.FullAccess}); err == nil {
		t.Fatal("confirmation replay")
	}
	w.Cancel()
	s, _ := owner.Attach("human")
	view, _ := s.Inspect(0, 128)
	if view.Permissions.Mode != permission.FullAccess {
		t.Fatal("detach revoked approved Session")
	}
	encoded, _ := json.Marshal(view)
	if strings.Contains(string(encoded), "Lease") {
		t.Fatal("private authority in public snapshot")
	}
	server := httptest.NewServer(Handler(Config{Host: owner}))
	defer server.Close()
	body, _ := json.Marshal(request{ID: "human", Generation: v.Generation, Permission: &permission.ControlRequest{Action: "watch"}})
	req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/permissions", bytes.NewReader(body))
	req.Host = "localhost"
	r, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	raw, _ := io.ReadAll(r.Body)
	if !bytes.Contains(raw, []byte("unauthenticated_control")) {
		t.Fatal("untrusted transport granted control", string(raw))
	}
	if _, err = client.WatchPermissions(t.Context(), "human", "old"); err == nil {
		t.Fatal("stale generation got authority")
	}
}

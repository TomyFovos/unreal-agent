package primitives_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/primitives"
)

func assertPermissionEvent(t *testing.T, events []primitives.PrimitiveEvent, code permission.Code) {
	t.Helper()
	if len(events) != 1 || events[0].Type != primitives.PrimitiveEventFailed {
		t.Fatalf("events=%#v", events)
	}
	failure, ok := events[0].Result.(primitives.PrimitiveFailureResult)
	if !ok || failure.Denial == nil || failure.Denial.Code != code {
		t.Fatalf("failure=%#v", events[0].Result)
	}
}

func TestPrimitivePolicyCannotBeBypassedDirectly(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	policy, err := permission.New(permission.Config{ReadRoots: []string{root}, WriteRoots: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	defer policy.Close()
	ctx := permission.WithPolicy(t.Context(), policy)
	assertPermissionEvent(t, collectEvents(readFile(ctx, primitives.IOReadRequest{Path: filepath.Join(root, "escape", "secret"), Count: 64})), permission.Denied)
	target := filepath.Join(root, "escape", "created")
	assertPermissionEvent(t, collectEvents(create(ctx, primitives.IOCreateRequest{Path: target, Kind: primitives.IOCreateRegularFile, Mode: 0600})), permission.Denied)
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("denied write had effects")
	}
	events := make(chan primitives.PrimitiveEvent)
	marker := filepath.Join(outside, "process-marker")
	primitives.StartProcess(ctx, primitives.ProcessStartRequest{Path: "/bin/sh", Arguments: []string{"-c", "touch " + marker}}, events)
	assertPermissionEvent(t, collectEvents(events), permission.Denied)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("denied subprocess ran")
	}
}

func TestPrimitiveRejectsUnsupportedSandboxBeforeCaptureSideEffects(t *testing.T) {
	directory := t.TempDir()
	capture := filepath.Join(directory, "capture")
	if err := os.WriteFile(capture, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	policy, err := permission.New(permission.Config{ProcessMode: permission.ProcessUnrestricted, ReadRoots: []string{directory}})
	if err != nil {
		t.Fatal(err)
	}
	defer policy.Close()
	events := make(chan primitives.PrimitiveEvent)
	primitives.StartProcess(permission.WithPolicy(t.Context(), policy), primitives.ProcessStartRequest{
		Path: "/bin/sh", Arguments: []string{"-c", "printf ran"}, StdoutPath: capture,
	}, events)
	assertPermissionEvent(t, collectEvents(events), permission.Unsupported)
	contents, err := os.ReadFile(capture)
	if err != nil || string(contents) != "untouched" {
		t.Fatal("denied process changed capture")
	}
}

func TestRemoteClientEnforcesRedirectPolicyAndRedactsDenials(t *testing.T) {
	var called atomic.Int32
	outside := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called.Add(1) }))
	defer outside.Close()
	allowed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, outside.URL+"/path?key=SECRET", http.StatusFound)
	}))
	defer allowed.Close()
	policy, err := permission.New(permission.Config{NetworkOrigins: []string{allowed.URL}})
	if err != nil {
		t.Fatal(err)
	}
	defer policy.Close()
	client := primitives.NewRemoteClient()
	defer client.Close()
	events := make(chan primitives.PrimitiveEvent)
	request := primitives.DefaultRemoteRequest("test", "request", allowed.URL)
	client.SendRequest(permission.WithPolicy(t.Context(), policy), request, events)
	collected := collectEvents(events)
	assertPermissionEvent(t, collected, permission.Denied)
	failure := collected[0].Result.(primitives.PrimitiveFailureResult)
	if strings.Contains(failure.Error, "SECRET") || called.Load() != 0 {
		t.Fatalf("policy leaked or bypassed: %s", failure.Error)
	}
}

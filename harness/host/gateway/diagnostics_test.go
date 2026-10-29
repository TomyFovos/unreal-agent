//go:build linux || darwin

package gateway

import (
	"errors"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
)

func TestGatewayOpenIncludesSafeUnderlyingReason(t *testing.T) {
	_, client, _, directory := fixture(t)
	store, err := localfile.New(directory)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.CreateWithHostRecords(t.Context(), "mismatch", sessionstore.HostRecord{Version: 1, Kind: "configuration", Configuration: []byte(`{"Workspace":"/original/workspace"}`)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Open(t.Context(), host.Resume, "mismatch")
	var diagnostic *Error
	if !errors.As(err, &diagnostic) || diagnostic.Code != "request_failed" || !strings.Contains(diagnostic.Reason, "session runtime configuration mismatch") || !strings.Contains(err.Error(), diagnostic.Reason) {
		t.Fatal("gateway discarded underlying resume failure", err)
	}
}

func TestGatewayReasonExcludesSecretsAndTerminalControls(t *testing.T) {
	for _, value := range []string{"workspace failed: token=synthetic-credential", "workspace failed: api_\x1bkey=synthetic-credential", "email=synthetic-private-identity"} {
		message := (&Error{Code: "request_failed", Reason: value}).Error()
		if strings.Contains(message, "synthetic-") || strings.ContainsRune(message, '\x1b') || !strings.Contains(message, "request_failed") {
			t.Fatal("unsafe diagnostic", message)
		}
	}
	if got := (&Error{Code: "request_failed", Reason: "session runtime configuration mismatch"}).Error(); got != "gateway: request_failed: session runtime configuration mismatch" {
		t.Fatal(got)
	}
}

func TestGatewayObservationVersionTracksStoppedSession(t *testing.T) {
	owner, client, _, _ := fixture(t)
	v, err := client.Open(t.Context(), host.Create, "version")
	if err != nil {
		t.Fatal(err)
	}
	before, err := client.Version(t.Context(), "version")
	if err != nil || before.Generation != v.Generation {
		t.Fatal(before, err)
	}
	s, err := owner.Attach("version")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Stop(t.Context(), s.Generation, inbox.StopHard, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	after, err := client.Version(t.Context(), "version")
	if err != nil || after.Generation != before.Generation || after.Revision <= before.Revision {
		t.Fatal("version failed to observe stop", after, err)
	}
}

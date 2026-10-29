package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/host/gateway"
)

func TestLauncherSessionErrorKeepsSafeGatewayCause(t *testing.T) {
	cause := &gateway.Error{Code: "request_failed", Reason: "session runtime configuration mismatch"}
	err := launcherSessionError("devlore-copy", cause)
	if strings.Contains(err.Error(), "request_failed") || strings.Contains(err.Error(), cause.Reason) || !errors.Is(err, cause) {
		t.Fatal("launcher discarded underlying cause", err)
	}
	unsafe := errors.New("workspace failed: token=synthetic-credential")
	err = launcherSessionError("copy", unsafe)
	if strings.Contains(err.Error(), "synthetic-credential") || strings.Contains(err.Error(), "workspace failed") || !errors.Is(err, unsafe) {
		t.Fatal("launcher diagnostic leaked or lost cause", err)
	}
}

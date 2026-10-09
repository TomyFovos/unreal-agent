package primitives

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/permission"
)

func TestRemoteFailureClosedKindsAndPermissionPrecedence(t *testing.T) {
	for _, test := range []struct {
		err  error
		kind RemoteFailureKind
	}{
		{context.DeadlineExceeded, RemoteFailureTimeout},
		{&net.DNSError{Name: "private-host", Err: "private-body", IsTimeout: true}, RemoteFailureTimeout},
		{&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("private-body")}, RemoteFailureConnection},
		{io.ErrUnexpectedEOF, RemoteFailureConnection},
		{x509.UnknownAuthorityError{}, RemoteFailureTLS},
		{errors.New("private-body timeout EOF"), RemoteFailureUnknown},
	} {
		e := remoteFailure(RemoteRequest{}, fmt.Errorf("private-wrapper: %w", test.err)).Result.(PrimitiveFailureResult)
		if e.RemoteKind != test.kind {
			t.Fatalf("wrong safe category %s", e.RemoteKind)
		}
	}
	denial := &permission.Error{Code: permission.Denied, Capability: "network"}
	failure := remoteFailure(RemoteRequest{}, denial).Result.(PrimitiveFailureResult)
	if failure.Denial == nil || failure.RemoteKind != "" {
		t.Fatal("permission failure replaced by transport classification")
	}
}

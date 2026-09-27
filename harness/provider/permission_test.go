package provider

import (
	"errors"
	"fmt"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"testing"
)

func TestNormalizeRetainsTypedPermissionFailure(t *testing.T) {
	want := &permission.Error{Code: permission.Denied, Capability: "network", Reason: "destination is not allowed"}
	got := normalize(fmt.Errorf("wrapped: %w", want))
	var typed *permission.Error
	if !errors.As(got, &typed) || *typed != *want {
		t.Fatalf("typed permission lost: %v", got)
	}
}

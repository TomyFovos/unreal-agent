package analysis

import (
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func TestRoutingCountsExistingPermissionDecisions(t *testing.T) {
	policyDenial := &permission.Error{Code: permission.Denied, Capability: "tool"}
	approvalDenial := &permission.Error{Code: permission.Denied, Capability: "approval"}
	for _, tc := range []struct {
		name   string
		mode   permission.Mode
		state  string
		denial *permission.Error
		want   string
	}{
		{"full access", permission.FullAccess, "full_access", nil, "A1"},
		{"read in read-only", permission.ReadOnly, "read_only", nil, "A1"},
		{"write in read-only", permission.ReadOnly, "read_only", approvalDenial, "A2"},
		{"tool disabled by policy", permission.Ask, "denied", policyDenial, "A2"},
		{"parent denied", permission.Ask, "denied", approvalDenial, ""},
		{"parent allowed", permission.Ask, "allow_once", nil, ""},
		{"waiting on parent", permission.Ask, "pending", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			op := operation.Operation{
				Type: operation.TypeShell, Status: operation.StatusCompleted,
				Permission: &permission.Decision{Mode: tc.mode, State: tc.state}, Denial: tc.denial,
			}
			if got := sessionstore.AutomaticPermissionRule(op); got != tc.want {
				t.Fatalf("approval routing = %q, want %q", got, tc.want)
			}
		})
	}
}

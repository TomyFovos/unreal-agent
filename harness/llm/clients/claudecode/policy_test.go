package claudecode

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestPolicyMetadataRequiresKnownAbsence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		reason policyReason
	}{
		{"source exists", nil, policySourcePresent},
		{"unreadable source", os.ErrPermission, policyStateUnknown},
		{"failed inspection", errors.New("private-policy-value"), policyStateUnknown},
		{"source absent", os.ErrNotExist, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := checkPolicyPaths([]string{"private-path"}, func(string) (os.FileInfo, error) { return nil, tc.err })
			if tc.reason == 0 {
				if e != nil {
					t.Fatal(e)
				}
				return
			}
			var typed *Error
			if !errors.As(e, &typed) || typed.Code != "policy_isolation_unavailable" || typed.policyReason != tc.reason {
				t.Fatal("policy evidence was lost", e)
			}
			if strings.Contains(e.Error(), "private-") {
				t.Fatal("inspection error leaked private data")
			}
		})
	}
}

func TestRemotePolicyEligibilityIsNotProofOfCurrentPolicy(t *testing.T) {
	for _, subscription := range []string{"pro", "max"} {
		if e := checkRemotePolicyIsolation(subscription); e != nil {
			t.Fatal("known fetch-ineligible stored login rejected", e)
		}
	}
	for _, subscription := range []string{"team", "enterprise", "", "unknown-private-plan"} {
		e := checkRemotePolicyIsolation(subscription)
		var typed *Error
		if !errors.As(e, &typed) || typed.Code != "policy_isolation_unavailable" {
			t.Fatal("unproven remote policy allowed", e)
		}
		want := policyStateUnknown
		if subscription == "team" || subscription == "enterprise" {
			want = policyRemoteMutable
		}
		if typed.policyReason != want || strings.Contains(e.Error(), subscription) && subscription != "" {
			t.Fatal("rejection used a plan name instead of isolation evidence", e)
		}
	}
}

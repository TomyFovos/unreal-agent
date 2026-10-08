package claudecode

import (
	"errors"
	"os"
)

// ManagedPolicyMode is an explicit administrative trust boundary, not a claim
// that Unreal verified the contents or side effects of organization policy.
type ManagedPolicyMode string

const (
	ManagedPolicyReject ManagedPolicyMode = "reject"
	ManagedPolicyTrust  ManagedPolicyMode = "trust"
)

func (m ManagedPolicyMode) Effective() ManagedPolicyMode {
	if m == "" {
		return ManagedPolicyReject
	}
	return m
}

func (m ManagedPolicyMode) Validate() error {
	switch m.Effective() {
	case ManagedPolicyReject, ManagedPolicyTrust:
		return nil
	default:
		return &Error{Code: "invalid_managed_policy_mode"}
	}
}

func checkSubscriptionPolicy(subscription string, mode ManagedPolicyMode) error {
	if err := mode.Validate(); err != nil {
		return err
	}
	if mode.Effective() == ManagedPolicyReject {
		return checkRemotePolicyIsolation(subscription)
	}
	// Authentication must still identify a supported first-party subscription.
	// No cached policy or doctor's point-in-time output authorizes this mode.
	switch subscription {
	case "pro", "max", "team", "enterprise":
		return nil
	default:
		return &Error{Code: "subscription_unavailable"}
	}
}

// Reasons identify unavailable isolation evidence, not a disallowed plan name.
// They are closed, nonsecret values and never contain settings/doctor output.
type policyReason uint8

const (
	policySourcePresent policyReason = iota + 1
	policyRemoteMutable
	policyStateUnknown
)

func policyFailure(reason policyReason) error {
	return &Error{Code: "policy_isolation_unavailable", policyReason: reason}
}

func checkPolicyPaths(paths []string, stat func(string) (os.FileInfo, error)) error {
	for _, path := range paths {
		_, err := stat(path)
		if err == nil {
			// Existence is not evidence that a policy is active, empty or safe.
			// Contents, including cached env/credential values, are never opened.
			return policyFailure(policySourcePresent)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return policyFailure(policyStateUnknown)
		}
	}
	return nil
}

func checkRemotePolicyIsolation(subscription string) error {
	// This is fetch eligibility under the verified first-party stored-login
	// contract (checked by prepare), not an entitlement/plan restriction. See
	// https://code.claude.com/docs/en/server-managed-settings#platform-availability
	// Pro/Max do not trigger remote settings delivery through that contract.
	switch subscription {
	case "pro", "max":
		return nil
	case "team", "enterprise":
		// doctor's "none configured for this organization" is only a snapshot.
		// A fresh -p process fetches again, and non-interactive runs apply newly
		// delivered executable settings. No official no-policy pin/lease exists
		// in the verified 2.1.285 contract. Do not launch doctor as a workaround:
		// its common settings initialization may load endpoint policy/helpers,
		// and WSL Windows/OS sources cannot be proven absent by Linux file stats.
		return policyFailure(policyRemoteMutable)
	default:
		return policyFailure(policyStateUnknown)
	}
}

//go:build linux || darwin

package claudecode

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

func TestDoctorSnapshotsCannotAuthorizeANewModelProcess(t *testing.T) {
	outcomes := []struct{ name, output string }{
		{"organization unconfigured", "Managed settings (remote): none configured for this organization"},
		{"loaded", "Managed settings (remote): loaded"},
		{"fetch failed", "Managed settings (remote): fetch failed — no policy applied (network error)"},
		{"cached policy", "Managed settings (remote): fetch failed — using stale cache (request timed out)"},
		{"skipped", "Managed settings (remote): not fetched — requires an Enterprise or Team subscription"},
		{"in progress", "Managed settings (remote): checking… (fetch in progress; re-run in a moment)"},
		{"unknown", "private-diagnostic-value"},
	}
	for _, subscription := range []string{"team", "enterprise"} {
		for _, outcome := range outcomes {
			t.Run(subscription+"/"+outcome.name, func(t *testing.T) {
				c, f := fakeClient(t)
				f.Set(t, testclaude.Config{Subscription: subscription, Doctor: outcome.output})
				// There is no policy/cache file in this fixture. Even a positive
				// snapshot cannot bind a subsequent process's new settings fetch.
				if _, e := os.Stat(filepath.Join(f.Home, ".claude", "remote-settings.json")); !errors.Is(e, os.ErrNotExist) {
					t.Fatal("fixture unexpectedly has cached policy", e)
				}
				for _, e := range []error{c.Probe(t.Context()), respondError(c, t)} {
					var typed *Error
					if !errors.As(e, &typed) || typed.Code != "policy_isolation_unavailable" || typed.policyReason != policyRemoteMutable {
						t.Fatal("point-in-time diagnostic authorized a model process", e)
					}
					if strings.Contains(e.Error(), "private-diagnostic-value") {
						t.Fatal("raw diagnostic leaked")
					}
				}
				if len(f.Calls(t)) != 0 {
					t.Fatal("unproven remote policy sent a model request")
				}
				if _, e := os.Stat(filepath.Join(f.Directory, "doctor-called")); !errors.Is(e, os.ErrNotExist) {
					t.Fatal("preflight executed an unproven policy-loading diagnostic", e)
				}
			})
		}
	}
}

func respondError(c *Client, t *testing.T) error {
	t.Helper()
	_, e := c.Respond(t.Context(), textRequest(), llm.RequestOptions{})
	return e
}

func TestExistingPolicySourceBlocksFetchIneligibleLogin(t *testing.T) {
	for _, subscription := range []string{"pro", "max"} {
		t.Run(subscription, func(t *testing.T) {
			c, f := fakeClient(t)
			f.Set(t, testclaude.Config{Subscription: subscription})
			dir := filepath.Join(f.Home, ".claude")
			if e := os.Mkdir(dir, 0700); e != nil {
				t.Fatal(e)
			}
			// Even an empty cached object is not an official absence attestation.
			if e := os.WriteFile(filepath.Join(dir, "remote-settings.json"), []byte("{}"), 0600); e != nil {
				t.Fatal(e)
			}
			e := c.Probe(t.Context())
			var typed *Error
			if !errors.As(e, &typed) || typed.policyReason != policySourcePresent || len(f.Calls(t)) != 0 {
				t.Fatal("plan name overrode actual policy evidence", e)
			}
		})
	}
}

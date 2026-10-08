//go:build linux || darwin

package claudecode

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

func TestProbeProductionEnvironmentRecognizesCLIStoredSubscription(t *testing.T) {
	for _, plan := range []string{"team", "enterprise"} {
		t.Run(plan, func(t *testing.T) {
			c, f := fakeClient(t)
			c.config.ManagedPolicyMode = ManagedPolicyTrust
			f.Set(t, testclaude.Config{Subscription: plan})
			c.config.Getenv = func(key string) string {
				switch key {
				case "CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST", "CLAUDE_CODE_SIMPLE":
					return "1" // An embedding parent must not take over CLI auth.
				case "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_HOST_AUTH_ENV_VAR", "CLAUDE_CODE_HOST_CREDS_FILE", "OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_HEADERS", "USER", "LOGNAME":
					return "parent-sensitive"
				}
				return f.Getenv(key)
			}
			if err := c.Probe(t.Context()); err != nil {
				t.Fatal("production environment hid a valid CLI-owned subscription login", err)
			}
			if len(f.Calls(t)) != 0 {
				t.Fatal("auth preflight sent a model request")
			}
			calls := f.AuthCalls(t)
			if len(calls) != 1 {
				t.Fatal("auth status was not probed exactly once")
			}
			call := calls[0]
			for _, env := range call.Environment {
				if strings.Contains(env, "parent-sensitive") || strings.HasPrefix(env, "CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST=") || strings.HasPrefix(env, "CLAUDE_CODE_SIMPLE=") || strings.HasPrefix(env, "ANTHROPIC_") || strings.HasPrefix(env, "OTEL_") {
					t.Fatal("subscription/credential/telemetry isolation regressed")
				}
			}
			for _, expected := range []string{"HOME=" + f.Home, "CLAUDE_CONFIG_DIR=" + filepath.Join(f.Home, ".claude"), "PATH=/usr/bin:/bin", "LANG=C.UTF-8", "TERM=dumb", "NO_COLOR=1", "CLAUDE_CODE_SAFE_MODE=1"} {
				if !slices.Contains(call.Environment, expected) {
					t.Fatal("auth probe lost intended production environment", expected)
				}
			}
			for _, flag := range []string{"--safe-mode", "--restricted", "--strict-mcp-config", "--disable-slash-commands", "--no-chrome"} {
				if !slices.Contains(call.Arguments, flag) {
					t.Fatal("auth probe lost isolation", flag)
				}
			}
			argument(t, call.Arguments, "--tools", "")
			argument(t, call.Arguments, "--disallowedTools", "*")
			argument(t, call.Arguments, "--mcp-config", `{"mcpServers":{}}`)
			argument(t, call.Arguments, "--setting-sources", "")
			argument(t, call.Arguments, "--system-prompt-file", os.DevNull)
			argument(t, call.Arguments, "--max-turns", "1")
			if !strings.HasPrefix(filepath.Base(call.Directory), "unreal-claude-probe-") {
				t.Fatal("auth preflight did not use a disposable private cwd")
			}
			if _, err := os.Stat(call.Directory); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("auth preflight directory was not removed", err)
			}
		})
	}
}

func TestAuthStatus285Normalization(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		mode       ManagedPolicyMode
		config     testclaude.Config
	}{
		{"Team trust", "", ManagedPolicyTrust, testclaude.Config{Subscription: "team"}},
		{"Enterprise trust", "", ManagedPolicyTrust, testclaude.Config{Subscription: "enterprise"}},
		{"Team reject", "policy_isolation_unavailable", ManagedPolicyReject, testclaude.Config{Subscription: "team"}},
		{"unsigned CLI exit 1", "external_reauth_required", ManagedPolicyTrust, testclaude.Config{SignedOut: true}},
		{"unsigned metadata exit 0", "external_reauth_required", ManagedPolicyTrust, testclaude.Config{AuthStatus: `{"loggedIn":false,"authMethod":"none","apiProvider":"firstParty","subscriptionType":null}`}},
		{"malformed JSON", "subprocess_failure", ManagedPolicyTrust, testclaude.Config{AuthStatus: "private-auth-sensitive malformed"}},
		{"malformed field type", "subprocess_failure", ManagedPolicyTrust, testclaude.Config{AuthStatus: `{"loggedIn":"true","authMethod":"claude.ai"}`}},
		{"third-party provider", "subscription_unavailable", ManagedPolicyTrust, testclaude.Config{APIProvider: "thirdParty", Subscription: "team"}},
		{"unknown subscription", "subscription_unavailable", ManagedPolicyTrust, testclaude.Config{Subscription: "unknown-private-sensitive"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, f := fakeClient(t)
			c.config.ManagedPolicyMode = tc.mode
			f.Set(t, tc.config)
			err := c.Probe(t.Context())
			if tc.code == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				requireCode(t, err, tc.code)
				if strings.Contains(err.Error(), "sensitive") {
					t.Fatal("auth-status details leaked")
				}
				if tc.code == "external_reauth_required" && !credential.IsCode(err, tc.code) {
					t.Fatal("external reauth semantics lost")
				}
			}
			if len(f.Calls(t)) != 0 {
				t.Fatal("auth preflight sent inference")
			}
		})
	}
}

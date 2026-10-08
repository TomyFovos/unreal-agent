//go:build linux || darwin

package claudecode

import (
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

func TestManagedPolicyModeDefaultsAndExplicitValidation(t *testing.T) {
	for _, tc := range []struct {
		mode ManagedPolicyMode
		want ManagedPolicyMode
	}{{"", ManagedPolicyReject}, {ManagedPolicyReject, ManagedPolicyReject}, {ManagedPolicyTrust, ManagedPolicyTrust}} {
		c, _ := fakeClient(t)
		config := c.config
		config.ManagedPolicyMode = tc.mode
		client, err := NewClient(config)
		if err != nil || client.config.ManagedPolicyMode.Effective() != tc.want {
			t.Fatal("wrong explicit trust/default boundary", err)
		}
		b, err := json.Marshal(config)
		if err != nil {
			t.Fatal(err)
		}
		if tc.mode == "" && strings.Contains(string(b), "managedPolicyMode") {
			t.Fatal("changed legacy creation identity for an omitted mode")
		}
		var restored Config
		if err = json.Unmarshal(b, &restored, json.RejectUnknownMembers(true)); err != nil || restored.ManagedPolicyMode.Effective() != tc.want {
			t.Fatal("lost policy mode on serialization", err)
		}
	}
	for _, mode := range []ManagedPolicyMode{"TRUST", "allow", "trust\n", "private-mode-sensitive"} {
		c, _ := fakeClient(t)
		config := c.config
		config.ManagedPolicyMode = mode
		_, err := NewClient(config)
		requireCode(t, err, "invalid_managed_policy_mode")
		if strings.Contains(err.Error(), string(mode)) {
			t.Fatal("invalid configuration value leaked")
		}
	}
}

func TestManagedPolicySubscriptionMatrix(t *testing.T) {
	for _, mode := range []ManagedPolicyMode{"", ManagedPolicyReject, ManagedPolicyTrust} {
		for _, plan := range []string{"pro", "max", "team", "enterprise"} {
			t.Run(string(mode)+"/"+plan, func(t *testing.T) {
				c, f := fakeClient(t)
				c.config.ManagedPolicyMode = mode
				f.Set(t, testclaude.Config{Subscription: plan})
				err := c.Probe(t.Context())
				allowed := mode == ManagedPolicyTrust || plan == "pro" || plan == "max"
				if allowed {
					if err != nil {
						t.Fatal("supported subscription rejected", err)
					}
				} else {
					requireCode(t, err, "policy_isolation_unavailable")
				}
				if len(f.Calls(t)) != 0 {
					t.Fatal("preflight sent inference")
				}
			})
		}
	}
}

func TestTrustDoesNotReadOrAuthorizeFromManagedCache(t *testing.T) {
	for _, plan := range []string{"team", "enterprise"} {
		t.Run(plan, func(t *testing.T) {
			c, f := fakeClient(t)
			c.config.ManagedPolicyMode = ManagedPolicyTrust
			f.Set(t, testclaude.Config{Subscription: plan, Doctor: "fetch failed — cached private-policy-sensitive"})
			dir := filepath.Join(f.Home, ".claude")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			// Authorization cannot inspect even a readable cached payload: this
			// FIFO has no writer, so opening contents would block the request.
			if err := syscall.Mkfifo(filepath.Join(dir, "remote-settings.json"), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if err := c.Probe(ctx); err != nil {
				t.Fatal("explicit trust depended on cached contents/diagnostics", err)
			}
			if _, err := c.Respond(ctx, textRequest(), llm.RequestOptions{}); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(f.Directory, "doctor-called")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("trust was inferred from doctor")
			}
		})
	}
}

func TestTrustRetainsAuthenticationBillingAndTextOnlyFailures(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		config     testclaude.Config
	}{
		{"not signed in", "external_reauth_required", testclaude.Config{SignedOut: true}},
		{"API auth", "subscription_unavailable", testclaude.Config{AuthMethod: "api_key"}},
		{"API provider", "subscription_unavailable", testclaude.Config{APIProvider: "thirdParty"}},
		{"unknown account", "subscription_unavailable", testclaude.Config{Subscription: "private-plan-sensitive"}},
		{"malformed auth status", "subprocess_failure", testclaude.Config{AuthStatus: "private-auth-sensitive malformed"}},
		{"unsupported CLI", "unsupported_version", testclaude.Config{Version: "2.1.284"}},
		{"missing tool isolation", "unsupported_version", testclaude.Config{MissingFlag: "--tools"}},
		{"malformed stream", "malformed_stream", testclaude.Config{Subscription: "team", Stream: "token-sensitive malformed\n"}},
		{"subprocess failure", "subprocess_failure", testclaude.Config{Subscription: "enterprise", Exit: 7}},
		{"direct tool output", "tools_unsupported", testclaude.Config{Subscription: "team", Stream: `{"type":"system","subtype":"init","model":"claude-configured-a","tools":[],"permissionMode":"default","mcp_servers":[]}
{"type":"assistant","message":{"id":"message","model":"claude-configured-a","content":[{"type":"tool_use","name":"Bash","input":{"command":"private-command-sensitive"}}]}}
`}},
		{"MCP exposed", "tools_unsupported", testclaude.Config{Subscription: "team", Stream: `{"type":"system","subtype":"init","tools":[],"mcp_servers":[{"name":"managed-mcp-sensitive"}]}
`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, f := fakeClient(t)
			c.config.ManagedPolicyMode = ManagedPolicyTrust
			f.Set(t, tc.config)
			_, err := c.Respond(t.Context(), textRequest(), llm.RequestOptions{})
			requireCode(t, err, tc.code)
			if strings.Contains(err.Error(), "sensitive") {
				t.Fatal("subprocess/configuration detail leaked")
			}
		})
	}
	c, f := fakeClient(t)
	c.config.ManagedPolicyMode = ManagedPolicyTrust
	f.Set(t, testclaude.Config{Subscription: "team"})
	r := textRequest()
	r.Tools = []llm.Tool{{Name: "Read"}}
	_, err := c.Respond(t.Context(), r, llm.RequestOptions{})
	requireCode(t, err, "tools_unsupported")
	if len(f.Calls(t)) != 0 {
		t.Fatal("Unreal tools bypassed text-only capability")
	}
	for _, key := range []string{"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY", "CLAUDE_CODE_USE_MANTLE", "CLAUDE_CODE_USE_ANTHROPIC_AWS", "ANTHROPIC_BASE_URL", "ANTHROPIC_CUSTOM_HEADERS"} {
		c.config.Getenv = func(k string) string {
			if k == key {
				return "private-routing-sensitive"
			}
			return f.Getenv(k)
		}
		requireCode(t, c.Probe(t.Context()), "subscription_mode_conflict")
	}
	c.config.Getenv = f.Getenv
	invalid := textRequest()
	invalid.Model.ID = "unconfigured-model-sensitive"
	_, err = c.Respond(t.Context(), invalid, llm.RequestOptions{})
	requireCode(t, err, "invalid_model")
	invalid = textRequest()
	invalid.Model.ReasoningEffort = "unconfigured-effort-sensitive"
	_, err = c.Respond(t.Context(), invalid, llm.RequestOptions{})
	requireCode(t, err, "invalid_effort")
	c.config.Binary = "/nonexistent/unreal-claude-test"
	requireCode(t, c.Probe(t.Context()), "binary_not_found")
	if len(f.Calls(t)) != 0 {
		t.Fatal("invalid model/effort/binary performed inference")
	}
}
